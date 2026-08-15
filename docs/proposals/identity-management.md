# Design Proposal: identity management scope & provider-driven gating

## 1. Problem

The server exposes an Identity API (`/api/v1/identity`) to manage users, groups and
memberships. It is hard-wired to **kubauth**: it reads/writes kubauth CRDs (`User`,
`Group`, `GroupBinding`) and is always exposed, whether or not kubauth is installed.

Production deployments are expected to bring their own identity provider (Keycloak,
Entra ID, LDAP-backed IdPs, Okta, ...). In these environments, the lifecycle of human
identities is managed **outside** OKDP, and the Identity API is at best dead surface, at
worst misleading. At the same time, kubauth remains valuable as an optional batteries-
included IdP for sandboxes and early deployments (local users, short time-to-market
before wiring the enterprise IdP).

Goal, per the direction agreed in #25:

- **BYO OIDC by default**. The control plane only assumes a standard OIDC issuer.
- **kubauth is an optional fallback**, never a hard dependency.
- **The Identity API is driven by the platform configuration**, active when kubauth is
  configured, cleanly disabled otherwise (endpoints off, UI hides the section).

## 2. Current state

- Authentication is already provider-neutral: the UI/portal uses the generic
  `defaultIdp` OIDC configuration from the Context.
- The Identity API (`internal/api/handlers/identity_handler.go`,
  `internal/service/identity_service.go`, `internal/repository/identity_repository.go`)
  manipulates kubauth CRDs in a namespace fixed at startup (`PLATFORM_NAMESPACE` env).
- On service deletion, the server best-effort deletes a kubauth `OidcClient` named after
  the release (`cleanupOidcClient`), unconditionally targeting kubauth.
- Nothing tells the UI whether user management is available: the Identity section is
  always shown.
- The console's OIDC client (issuer, client id) is fixed at build time, so pointing a
  console at another platform means rebuilding the image.

## 3. Proposal

### 3.1 Context contract

All identity-related configuration moves under one `identity` subtree of the Context:

```yaml
identity:
  # Who owns human identities. "external" (default): BYO OIDC, OKDP never
  # manages users/groups. "kubauth": the optional built-in IdP is installed
  # and the Identity API is exposed.
  provider: external | kubauth

  # kubauth adapter configuration (required when provider: kubauth).
  kubauth:
    namespace: kubauth

  # OIDC client the console UI authenticates with. Optional: when unset, the
  # UI keeps its build-time configuration.
  oidc:
    authority: https://keycloak.example.com/realms/okdp
    clientId: okdp-console
    scope: openid profile email groups   # optional, overrides the UI default

  # OIDC client provisioning: the only dynamic interaction the server has
  # with the IdP (registering/unregistering clients for deployed services).
  provisioning:
    provider: none | kubauth | keycloak   # default: none
    keycloak:
      issuerUri: https://keycloak.example.com/realms/master
      insecureSkipTLSVerify: false
      credentialsSecret:
        namespace: keycloak
        name: creds-okdp-provisioner
        # keys default to client_id / client_secret
```

Everything is resolved from the Context **at request time**: operators can switch
providers declaratively, without restarting the server.

### 3.2 Behavior matrix

| `identity.provider` | `/api/v1/identity` | UI Identity section | `provisioning.provider` | OIDC client cleanup on service delete |
|---|---|---|---|---|
| `external` (default) | 501, endpoints off | hidden | `none` (default) | no-op |
| `external` | 501 | hidden | `keycloak` | Keycloak Admin API |
| `kubauth` | active (kubauth CRDs) | shown | `kubauth` | kubauth `OidcClient` CRD |

The two axes are independent: e.g. an external Keycloak for identities
(`provider: external`) can still get dynamic client registration
(`provisioning.provider: keycloak`).

### 3.3 Capabilities discovery

New endpoint for the UI (and any API consumer) to discover what is available:

```
GET /api/capabilities
{
  "identity": {
    "provider": "external",
    "userManagement": false,
    "oidc": {
      "authority": "https://keycloak.example.com/realms/okdp",
      "clientId": "okdp-console",
      "scope": "openid profile email groups"
    }
  },
  "oidcProvisioning": { "provider": "keycloak" }
}
```

The UI shows/hides the Identity section based on `identity.userManagement`, and builds its
OIDC client from `identity.oidc`, so the console is configured by the platform instead of
hard-coding an issuer and client at build time. `identity.oidc` is omitted when the Context
does not set both `authority` and `clientId`. The UI then falls back to its build-time
configuration, which keeps existing deployments working.

### 3.4 What the Identity API is (and is not)

The Identity API stays **openly kubauth-specific**. No neutral users/groups abstraction
is introduced: enterprise IdPs are not managed through OKDP, so an abstraction would
have exactly one implementation. The provider-neutral extension point is the
`OidcClientProvisioner` contract (§3.5), which is the only dynamic interaction OKDP
needs with an IdP.

### 3.5 OIDC client provisioning contract

```go
type OidcClientProvisioner interface {
    EnsureClient(ctx context.Context, spec OidcClientSpec) error
    DeleteClient(ctx context.Context, name string) error
}
```

Implementations are per-IdP adapters selected per call from the Context:
- `none`: no-op, the default. Client registration is handled by an external IT process or
  declarative seeding.
- `kubauth`: manages `OidcClient` CRDs in `identity.kubauth.namespace`.
- `keycloak`: Keycloak Admin REST API, service-account credentials from a Kubernetes
  Secret.

`cleanupOidcClient` (service deletion) goes through this contract instead of talking to
kubauth directly.

## 4. Implementation sketch

Handler → service → repository pattern, no breaking change:

- `internal/repository/context_repository.go`: `GetIdentityProvider`,
  `GetIdentityOidcConfig`, `GetIdentityProvisioningProvider`,
  `GetKeycloakProvisioningConfig`. `GetKubauthNamespace` reads
  `identity.kubauth.namespace` (legacy `kubauth.namespace` still honored).
- `internal/service/capability_service.go`: derives capabilities from the Context.
- `internal/api/handlers/capabilities_handler.go`: `GET /api/capabilities` +
  `RequireIdentityAPI()` gin middleware gating the `/api/v1/identity` group (501 when
  `identity.provider != kubauth`, or when it is kubauth and the CRDs are absent).
- `internal/repository/provisioning/`: `OidcClientProvisioner` contract, `none`/
  `kubauth`/`keycloak` adapters, Context-driven selector.
- `internal/repository/identity_repository.go`: kubauth namespace resolved per call
  (Context first, `PLATFORM_NAMESPACE` fallback) instead of fixed at startup.

## 5. Compatibility

- Existing kubauth-based environments keep working: set `identity.provider: kubauth`
  (and optionally `identity.kubauth.namespace`, the legacy `kubauth.namespace` key and
  `PLATFORM_NAMESPACE` fallback being preserved).
- Environments without kubauth get a 501 on the Identity endpoints, where they used to
  fail against missing CRDs.
- No endpoint is removed. No request or response schema changes.

## 6. Decisions

| # | Question | Decision |
|---|----------|----------|
| 1 | Remove or gate the Identity API? | **Gate.** kubauth remains a supported optional fallback (#25). Removal would break sandbox/time-to-market use cases. |
| 2 | Neutral users/groups abstraction? | **No.** The API stays kubauth-specific. Only OIDC client provisioning gets a provider-neutral contract. |
| 3 | Gate resolution | Per request from the Context (no restart to switch providers). Cost: one Context GET per identity/capabilities call, acceptable and consistent with the rest of the server. |
| 4 | Disabled response code | `501`, carrying the `FeatureUnavailable` body the other optional features already answer (`reason: feature-not-installed`) and pointing to `/api/capabilities`. A 404 would not be distinguishable from a wrong path. |
| 5 | `EnsureClient` call sites | Not wired yet: packages register their own clients declaratively today. The contract ships now so adapters are complete. Wiring at deploy time is follow-up work. |

## 7. Implementation status

- ✅ Context contract getters (`identity.provider`, `identity.oidc`,
  `identity.provisioning.*`)
- ✅ `GET /api/capabilities` + gating middleware on `/api/v1/identity`
- ✅ `internal/repository/provisioning/` (none/kubauth/keycloak + selector + tests)
- ✅ Context-driven kubauth namespace in the identity repository
- ✅ UI: consume `/api/capabilities` to hide the Identity section and configure the OIDC
  client (separate PR on okdp-control-plane-ui)
- ⬜ Wire `EnsureClient` at service deploy time (follow-up)
