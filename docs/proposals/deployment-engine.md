# Design Proposal: a pluggable deployment engine

> Status: **IMPLEMENTED for KuboCD**, the only engine, with its desired state in the
> cluster (`kubernetes`) or in git (`git`, sections 4 to 6).

**TL;DR**

- The server calls its deployment engine through four Go interfaces (`DesiredState`,
  `Observer`, `Catalog`, `PlatformConfig`). KuboCD implements them twice: on the
  cluster, and on a git branch that Flux applies.
- In git mode every declarative object the server writes in a project (namespace,
  `Release`, `Context`, `Connection`, `SecretStore`, `ExternalSecret`) is a commit. The
  cluster only receives what Flux applies. Credentials `Secret`s never go to git.
- The public API does not say where the state lives: a deletion applied later answers
  `202` with its operation, an uncertain write `504`, `/api/capabilities` has no
  persistence block.
- The chart renders the Flux objects with their own ServiceAccount, keeps them on
  uninstall by default, and restricts the server's ClusterRole to reads on the kinds
  Flux applies. Back in kubernetes mode, the server deletes those Flux objects.

```
                 git mode                                     kubernetes mode
 console ──HTTP──▶ server ──commit──▶ git branch            console ──HTTP──▶ server
                    │  ▲                 │                                       │
           RequestSync  WatchSync     fetch                              create/update
                    ▼  │                 ▼                                       ▼
                  GitRepository ──▶ Kustomization ──apply──▶ cluster ◀───────────┘
                                   (SA <fullname>-gitops)      │
                    server ◀──────────── observe ──────────────┘
```

| Section | |
|---|---|
| [1. Shape](#1-shape) | the four interfaces |
| [2. Configuration](#2-configuration) | engine section, the two `Context`s |
| [3. API](#3-api) | phases, revisions, operations, status codes |
| [4. Git building blocks](#4-git-building-blocks) | `gitstore`, `gitops` |
| [5. Git persistence](#5-git-persistence-internalenginekubocdgit) | `kubocdgit` |
| [6. Server and chart](#6-server-and-chart) | startup, RBAC, mode switch |

---

## 1. Shape

The core of the server (handlers, authentication, parameter validation, pods, logs,
metrics) calls the deployment engine through four Go interfaces in
`internal/engine`:

| Interface | Role |
|---|---|
| `DesiredState` | create, update and delete instances and catalog entries. A write answers an `Operation` and an error. The error reports a failure unrelated to the content, the operation is then meaningless. Otherwise the operation is `accepted` (with the desired revision when the engine has one), `rejected` (with problems) or `unknown` (the write may have landed, read again) |
| `Observer` | read instances, their phase, revisions and connection bindings, the workloads they run, and a watch |
| `Catalog` | deployable services, their versions, parameter schema and connection inputs |
| `PlatformConfig` | ingress suffix, profile images, Spark settings |

In Kubernetes mode (`internal/engine/kubocd`), an instance is a `Release` in its
project namespace, the service catalog is stored in the Control Plane `Context`, and
infrastructure settings are read from the platform `Context`.
`internal/engine/fake` is an in-memory engine that behaves like a git-backed one
(commit-like revisions, instances `Pending` until it acts). `internal/engine/enginetest`
contains the shared behavior tests run against both implementations.

Not behind those interfaces yet: the connections CRUD (KuboCD `Connection` CRDs), projects
(plain namespaces), the identity settings read from the `Context`, the embedded
contracts, and the Flux label used to match an Ingress to an instance.

## 2. Configuration

`ENGINE_TYPE` (default `kubocd`) selects the engine. The kubocd engine reads and
validates its own section (backend, persistence, release interval and timeout) at
startup, before the server reaches the cluster. The `Context` settings stay in the core
configuration, which the identity and authentication code reads too. See the README
Configuration section for the variables.

Two `Context`s hold two kinds of settings:

| `Context` | Variables | Holds | Written by the server |
|---|---|---|---|
| platform | `PLATFORM_CONTEXT_NAME`, `PLATFORM_CONTEXT_NAMESPACE` | `ingress`, `oidc`, `identity`, `jupyter`, `sparkOperator` | never |
| control plane | `CONTROL_PLANE_CONTEXT_NAME`, `CONTROL_PLANE_CONTEXT_NAMESPACE` | `serviceCatalog` | catalog writes |

For upgrades from earlier releases that only set `CONTEXT_NAME` and `CONTEXT_NAMESPACE`,
those legacy variables act as a silent fallback for `PLATFORM_CONTEXT_NAME`,
`PLATFORM_CONTEXT_NAMESPACE`, and `CONTROL_PLANE_CONTEXT_NAMESPACE` when the newer
variables are unset. `repository.NewSplitContextRepository` reads the catalog from the
Control Plane `Context`, and from the platform one while the Control Plane `Context` is
missing or has no `serviceCatalog` (a transient API error is returned, never answered from
the platform). `NewSplitContextWriterRepository` writes the Control Plane `Context` only,
creates it when it is missing, and seeds a missing `serviceCatalog` from the platform one.
In kubernetes mode, if both `Context` references have the same name and namespace, that
single `Context` holds both infrastructure settings and the service catalog. In git mode,
the server refuses to start if both references point to the same `Context` because the
Control Plane `Context` is committed to git while the platform `Context` stays read-only.

## 3. API

Each service instance carries, besides its historical `status`:

- `phase`: `Pending`, `Installing`, `Updating`, `Ready`, `Degraded`, `Error`,
  `Suspended`, `Deleting` or `Unknown`.
- `desiredRevision`, `publishedRevision`, `appliedRevision`: opaque, empty when the
  engine cannot tell. KuboCD fills `desiredRevision` with the `Release` generation and
  `appliedRevision` with `status.observedGeneration` when reported (`> 0`).
- `connections[].state`: `declared`, `published`, `bound` or `ready`, absent when unknown.

On the instance routes, a successful create or update also carries
`operation: {outcome, desiredRevision}`. A failed request keeps its status code and
`error` text. When the server can classify the failure it adds `code`, and for a rejected
or unknown write the `operation`. `PATCH .../parameters` and `DELETE .../services/{name}`
accept `If-Match: "<desiredRevision>"` and answer 409 `conflict` when it is stale.

`DELETE .../services/{name}` answers `204` when the deletion is done in the cluster, and
`202 {"operation": {"outcome": "accepted", "desiredRevision": ...}}` when it is only
committed. Storage errors are mapped consistently across the service, catalog, project,
connection, secret store and external secret routes: `504` when a write outcome is unknown
(`engine.UnknownError`, `gitstore.ErrUncertain`), `503` when the backing store is
unavailable, busy, or waiting for Flux to create the project namespace
(`gitstore.ErrUnavailable`, `gitstore.ErrBusy`, `engine.ErrNamespaceProvisioning`), `502`
when the remote Git server rejects a push (`gitstore.ErrRejected`), and `409` on a
conflict or duplicate.

`GET /api/capabilities` describes the engine, never where it stores the desired state:

```json
{
  "engine": { "name": "kubocd", "renderedValues": false, "suspend": false }
}
```

The console tests these flags, never `engine.name`.

## 4. Git building blocks

Two packages the git-backed engine builds on.

`internal/gitstore` stores files under `<path>` on one branch of a git remote, in pure
Go (go-git, the image has no git binary):

- Layout under `<path>`, one file per object:

  ```
  contexts/<name>.yaml                        control plane Context (control-plane.yaml)
  projects/<project>/namespace.yaml
  projects/<project>/releases/<name>.yaml
  projects/<project>/connections/<name>.yaml
  projects/<project>/secret-stores/<name>.yaml
  projects/<project>/external-secrets/<name>.yaml
  ```

  Projects live under `projects/`, so a project named `contexts` cannot collide with
  the control plane Contexts. The platform `Context` stays out of git. The path helpers
  refuse any name that is not a Kubernetes name, and every path that could leave
  `<path>` or reach `.git`.
- A write is one commit of one or more files, authored by the caller's OIDC identity
  and committed by the control plane. `ExpectedRevision` (the `If-Match` value) is the
  revision of the file itself, the last commit that changed it. When the branch moved
  under a write, the write is replayed on the new head: writes on different files all
  succeed, two writes on one file with the same expected revision give one success and
  one conflict.
- `IdempotencyKey` is recorded in the commit, so a replayed create answers the first
  result after a restart or on another replica.
- A failed push is classified by the state of the remote: landed, lost (replayed),
  refused (`ErrRejected`), or unknown (`ErrUncertain`, the `unknown` outcome). The
  branch must be protected against force pushes: a rewind can let a push reported
  lost land later.
- `Bootstrap` commits `<path>/.gitkeep` on an empty remote or a missing path, without
  force and without touching existing content. A write that would empty `<path>` keeps
  `<path>/.gitkeep` in the same commit, so the path the sync tool reads never
  disappears.
- Credentials come from a Secret in the Flux `GitRepository` format, so the server and
  Flux can share it: `username`/`password`, `bearerToken`, `ca.crt`, or SSH `identity`
  with a mandatory `known_hosts`. `Config.CredentialsProvider` is called before every
  fetch and push, and `DirCredentialsProvider` resolves Kubernetes Secret `..data` once
  for atomic reads so a rotated Secret applies without a restart. Mounted `known_hosts`
  files are read directly without writing to `/tmp`, SSH config files from `~/.ssh/config`
  are disabled, and both the TCP dial and SSH handshake honor the request deadline. A URL
  holding a password, or a user over HTTP, is refused. Over HTTP, a redirect to another
  scheme, host or port is refused, so no credentials can follow it.

`internal/gitops` defines `SyncAdapter`, the neutral contract with the tool that applies
the branch (Flux, Argo CD): `Validate` at startup, `RequestSync` after a push (best
effort), `ObserveSync` and `WatchSync` for the applied revision and the sync phase
(`Unknown`, `Progressing`, `Ready`, `Failed` with the attempted revision, `Suspended`).
`internal/gitops/fake` is an in-memory adapter for tests.

## 5. Git persistence: `internal/engine/kubocdgit`

The KuboCD engine with its desired state in git, selected by
`ENGINE_KUBOCD_PERSISTENCE_MODE=git` (section 6).

- **Files.** One flat manifest per object, in the layout of section 4. An instance is
  `projects/<project>/releases/<project>-<name>.yaml`, the instance name a DNS-1123 label
  (63 characters at most). A write edits the file in place
  (yaml.v3 nodes): fields, order and comments it does not own stay, except the comments
  inside `serviceCatalog.categories`, which a catalog write re-encodes. Updates and catalog
  writes compare against the revision they read and start again on a conflict.
- **Writes** are authored by the caller (`auth.FromContext`, set by the authentication
  middleware), wake the sync tool up (`RequestSync`, best effort: the write waits 50ms
  for it at most, it goes on in the background up to `Deps.SyncTimeout` (3s), even when
  the caller's request was cancelled), and map the store
  errors to the engine outcomes: `ErrAlreadyExists` gives `instance-exists`,
  `ErrConflict` and a malformed `If-Match` give `conflict`, `ErrRejected` gives
  `rejected`, `ErrUncertain` gives `unknown`. An idempotency key is scoped to the
  instance. A replay after the catalog repository changed renders other bytes and is a
  `conflict`.
- **Reads** use a cached snapshot of the branch. A local write moves it to its commit, a
  new revision reported by `WatchSync` (or one the sync state names and the snapshot
  lacks) makes the next read fetch, and a TTL bounds its age. A read refreshing an
  expired snapshot waits `Deps.ReadFetchTimeout` (5s) at most. A failed or timed out
  fetch serves the last snapshot for another TTL. A read that must see the head
  (`ConnectionRepository.Get`, the `Get` of the secret stores, an observation behind the
  sync tool) fetches whatever the age of the snapshot, bounded the same way. The first
  fetch, with nothing to serve, and the reads of a write wait for the store's own timeout.
- **Observation** merges git with the project `Release`s of the namespace. A
  `Release` pushed to git without `okdp.io/*` labels is the instance named after it, without the
  `<project>-` prefix, of the service named by the last segment of its package
  repository. In git and not in the cluster: `Pending`. In the cluster and no longer in
  git: `Deleting` when it carries this Control Plane's sync mark (matching the configured
  Flux `Kustomization`, or any Flux/Argo CD mark when none is named), shown as `Deleting`
  when labelled `okdp.io/project: <project>` with a deletion timestamp, shown as it is
  when labelled `okdp.io/project: <project>` without one, and ignored otherwise. When
  cluster `Release`s cannot be listed, known instances stay `Unknown` rather than emitting
  a false deletion. `Ready` needs the cluster spec to match git (package repository, tag and
  `insecure`, effective target namespace, Contexts, `protected` when git sets it,
  parameters), the file revision
  to be an ancestor of the applied revision when the sync tool reports one,
  `status.observedGeneration` to have caught up when reported (`> 0`, falling back to phase
  and spec on older controllers that leave it at `0`), and the `Ready` condition not `False`.
- **Revisions.** `desiredRevision` is the revision of the file. `appliedRevision` equals it
  once the applied revision contains it (`IsAncestor`), the `Release` matches git and
  `observedGeneration == generation`. A failed sync is reported on the instances not
  applied yet, as `Error` when the attempted revision contains their file. Sync failure
  messages that name another project or namespace are redacted to a neutral message.
- **Catalog.** Read from `contexts/<CONTROL_PLANE_CONTEXT_NAME>.yaml` in git, falling back
  to the platform reader while the file is missing or lacks `serviceCatalog`.
  `BootstrapControlPlaneContext` commits that file once, from the Kubernetes
  `serviceCatalog` (`repository.ReadServiceCatalog`) or empty, and never overwrites it.
  Uniqueness and existence are enforced inside the CAS mutation (duplicate create returns
  conflict `409`, missing update or delete returns not found `404`). `ContextRepository()`
  serves the core with the catalog from git and everything else from the platform `Context`.
- **Projects** (`ProjectRepository`): `projects/<name>/namespace.yaml`, labelled
  `okdp.io/project: <name>`. Lists git and the
  cluster (skipping malformed or mismatched namespace manifests with a warning), refuses a
  name the cluster already uses, refreshes before concluding not found on `Get`, and
  deletes `projects/<name>/` in one commit (`gitstore.Change.DeleteTree`). A project only
  the cluster holds is listed, not changed.
- **Connections** (`ConnectionRepository`): `projects/<project>/connections/<name>.yaml`,
  merged with the cluster for their status, `CreationTimestamp`, and for the `Connection`s
  a `Release` publishes. `Get` gives the file revision as `resourceVersion`, and an `Update`
  from an older read is a `Conflict` (409). On credential updates in git mode, `CheckUpdate`
  verifies the `Connection` is in git before writing a fresh rotated Secret
  (`<name>-credentials-rot-<random>`) that merges preserved keys from the previous owned
  Secret, releasing the new Secret on definite commit failure or the previous Secret after a
  successful commit. `ClusterConnection`s and the credentials `Secret`s stay in the cluster.
- **SecretStores and ExternalSecrets** (`SecretStoreRepository`, `ExternalSecretRepository`):
  `projects/<project>/secret-stores/` and `external-secrets/`, merged with the status of
  the cluster. An update edits the fields the console owns (Vault server, path, version,
  CA bundle, authentication, default label, data entries matched by `secretKey`) and
  keeps the others. A store labelled default takes the label from the others in the same
  commit. An object only the cluster holds is listed, never changed. The Vault token
  `Secret` stays in the cluster, labelled `app.kubernetes.io/managed-by: okdp-control-plane`.
  A new token goes to a fresh `<store>-credentials-rot-<random>` that no store reads yet,
  so neither a failed commit nor a rotation not applied yet changes a live token. If the
  commit fails definitively, the server marks the unused new `Secret` for cleanup, and
  after a successful commit it marks the previous `Secret` for cleanup (`CredentialsReaper`
  deletes marked `Secret`s only when neither git manifests nor cluster objects reference
  them). In kubernetes mode, such a `Secret` is deleted once the store stops reading it.
- **Credentials.** A credentials `Secret` the server owns is never deleted with the commit
  that drops its `Connection` or `SecretStore` (deletion, credential rotation, switch from
  token to kubernetes auth): once the commit succeeded, it is labelled
  `okdp.io/orphaned-credentials`, never on an uncertain write. `CredentialsReaper` deletes
  it once no `Connection` and no `SecretStore` references it, in git (fetched, never cached,
  every `.yaml`/`.yml` of the two directories readable) and in the cluster, conditioned on
  the `Secret` being the one it read. Any doubt keeps the `Secret`s of the namespace. A
  cluster that does not serve `Connection`s or `SecretStore`s holds none.

## 6. Server and chart

`cmd/server` builds the desired state stores from the mode (`buildDesiredState`):

| | kubernetes | git |
|---|---|---|
| projects, Connections, SecretStores, ExternalSecrets | cluster | `kubocdgit` repositories |
| catalog | Control Plane `Context` in the cluster | `contexts/<CONTROL_PLANE_CONTEXT_NAME>.yaml` in git |
| platform settings | platform `Context` | platform `Context` |
| background | none | sync watch, credentials reaper (30s) |

In git mode the startup is: verify the Control Plane and platform `Context` references are
distinct, open the store (30s network timeout, credentials from
`ENGINE_KUBOCD_PERSISTENCE_GIT_CREDENTIALS_DIR` validated once at startup even when
`initialize` is false), validate the Flux objects with sync `flux` (waiting 15s for objects
not created yet, failing at once on a mismatch or on missing `prune: true`, non-empty
`targetNamespace`, `kubeConfig`, or `..` path segments), then with `initialize` commit the
branch layout and the Control Plane `Context`. On `SIGINT`/`SIGTERM`, open SSE and
log-follow streams close immediately while in-flight REST requests drain within the 35s
shutdown window, followed by background loops and sync requests finishing cleanly.

The chart renders, in git mode and with `createSyncResources`:

```
ServiceAccount, ClusterRole, ClusterRoleBinding   <fullname>-gitops   (Flux impersonates it)
GitRepository                                     <fullname>          url, branch, secretRef
Kustomization                                     <fullname>          path, prune, wait: false,
                                                                      deletionPolicy: Orphan
all five: label app.kubernetes.io/managed-by-control-plane: <fullname>
          annotation helm.sh/resource-policy: keep   (keepSyncOnUninstall, default)
```

`wait: false` keeps a failing `Release` of one project from holding the applied revision
of the others. The server's ClusterRole keeps every right in kubernetes mode. In git mode
it reads namespaces, `Release`s, `Context`s, `Connection`s, `SecretStore`s and
`ExternalSecret`s, drops `delete` on `pods`, `persistentvolumeclaims`, and
`kubauth.kubotal.io/oidcclients`, and keeps the credentials `Secret`s and
`ClusterConnection`s (platform-wide, outside git). A Role in the release namespace, limited
to the objects named after the release, lets it read the Flux objects and patch the
`GitRepository` in git mode with sync `flux`, and only read and delete them in kubernetes
mode. That Role and RoleBinding are omitted in git mode with `sync: none`. With
`sync: none` or `createSyncResources: false` the chart renders no Flux object, and with
`sync: flux` and `createSyncResources: false` the server requires `serviceAccountName`,
`prune: true`, empty `targetNamespace`, and no `kubeConfig` on the externally supplied
`Kustomization`.

Switching back from git to kubernetes with `helm upgrade`: the chart stops rendering the
Flux objects (kept by the resource policy), and the server deletes at startup the
`Kustomization`, then the `GitRepository`, named
`ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC_CLEANUP_NAME` when they carry the label with that
value. Their `deletionPolicy: Orphan` leaves the projects, services and volumes in place,
and Flux no longer writes in competition with the server. The server refuses to start while
that `Kustomization` is not `deletionPolicy: Orphan` (Flux would delete what it applied),
cannot be read or deleted, or is still there 30s after its deletion. A `GitRepository` it
cannot delete is only logged: nothing applies it any more. The `<fullname>-gitops`
`ServiceAccount`, `ClusterRole`, and `ClusterRoleBinding` stay in the cluster until removed
manually, because the server's RBAC intentionally does not grant RBAC management.
