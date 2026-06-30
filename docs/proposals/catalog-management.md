# Design Proposal — Manage the service catalog from the Control Plane

> Issue: OKDP/okdp-control-plane-server#13 · UI counterpart: OKDP/okdp-control-plane-ui#23
> Status: **DRAFT — for discussion before implementation**

## 1. Problem

The catalog of deployable services (which services are exposed, in which versions, pointing to
which packages) is **read-only** in the Control Plane. The server reads it from the in-cluster
KuboCD `Context` and serves it through `GET /api/platform-services`. Changing anything (exposing
a new service, adding a version, fixing a package reference) requires hand-editing the cluster
`Context` with `kubectl`.

Goal: expose **write** operations so the catalog can be managed from the server API (and, on top
of it, the UI admin space — ui#23).

## 2. Current state (read path)

- Source of truth: the KuboCD `Context` CR (`kubocd.kubotal.io/v1alpha1`), name/namespace from
  `CONTEXT_NAME` / `CONTEXT_NAMESPACE` (defaults `default` / `kubocd-system`).
- The catalog lives under `spec.context.okdp.services[]`, each entry:
  ```yaml
  - name: trino
    versions: ["0.3.0", "0.2.0", "0.1.0"]
    default: "0.3.0"
    description: "Distributed SQL query engine"
    icon: "pi-search"
    category: "data-querying"
  ```
  plus `spec.context.okdp.packageRepository` (e.g. `quay.io/okdp/packages-dev`).
- Read code: `internal/repository/context_repository.go` (`GetPlatformServices`, `GetCatalog`,
  `GetPackageRepository`), `internal/service/service_service.go`, handler routes in
  `internal/api/router/router.go` (`GET /api/platform-services`, `/:name/versions`, `/:name/schema`).
- There is already a `context_writer_repository.go` that clones/syncs/deletes per-project Contexts,
  but it cannot edit the catalog granularly.

## 3. Proposed API

Mirror the existing read routes with write verbs (same `/api/platform-services` group):

| Method | Path | Purpose |
|--------|------|---------|
| `POST` | `/api/platform-services` | Expose a new service in the catalog |
| `PUT` | `/api/platform-services/:name` | Update a service (description/icon/category/default/versions) |
| `DELETE` | `/api/platform-services/:name` | Remove a service from the catalog |
| `POST` | `/api/platform-services/:name/versions` | Add a version |
| `DELETE` | `/api/platform-services/:name/versions/:tag` | Remove a version |

### Payloads

`POST /api/platform-services`
```json
{
  "name": "trino",
  "versions": ["0.3.0"],
  "default": "0.3.0",
  "description": "Distributed SQL query engine",
  "icon": "pi-search",
  "category": "data-querying"
}
```

`PUT /api/platform-services/trino` — same shape, all fields optional except those being changed.

`POST /api/platform-services/trino/versions`
```json
{ "tag": "0.4.0", "setDefault": true }
```

Responses: the updated `PlatformService` (200) for POST/PUT; 204 for DELETE. Errors use the
existing JSON error shape (`{"error": "..."}`), with appropriate status codes (see §5).

## 4. Where it writes

- All writes target the **`default` Context** — the single source of truth. Per-project Contexts
  are derived from it via `SyncFromDefault`, so editing a project Context would be overwritten;
  the catalog must be managed on `default` only.
- **Read-modify-write** on `spec.context.okdp.services`: read the CR, mutate the slice in memory,
  write it back with `Update`, wrapped in `client-go/util/retry.RetryOnConflict` to handle
  concurrent edits (`resourceVersion` conflicts).
- Implemented by extending `ContextWriterRepository` with `AddService` / `UpdateService` /
  `RemoveService` / `AddVersion` / `RemoveVersion` (granular, never a full `spec.context` overwrite).

## 5. Validation rules

Enforced in the service layer (`service_service.go`), returning **400** on violation:

- `name` is required, unique in the catalog, and a valid DNS-style identifier.
- `default` must be one of `versions`.
- `versions` must be non-empty and contain no duplicates.
- Each version `tag` **must exist in the OCI registry** for that package — reuse the existing
  `package_schema_service.listOCITags` so we never expose a version that can't be deployed.
- The per-service **`repository`** override (added in #22) is honored: versions are validated
  against that registry when set, otherwise against the Context's global `packageRepository`. The
  override round-trips through create/update/delete.
- `DELETE` a service / version that does not exist → **404**.
- Removing the `default` version → reject (**400**) unless a new default is provided.

## 6. Out of scope (this proposal)

- Managing `packageRepository` itself (the global registry URL) — could be a follow-up. (The
  per-service `repository` override is supported, since #22 added it to the model.)
- Editing per-project catalogs.
- Catalog categories (`okdp.catalogs`) management — separate concern, propose later.

## 7. RBAC / operational note — ALREADY COVERED BY THE SERVER CHART

In-cluster, write operations require `update`/`patch` on `contexts.kubocd.kubotal.io`. The server's
`ServiceAccount` + `ClusterRole` now live in this repo's Helm chart (added by #19), and the
`ClusterRole` (`chart/templates/rbac.yaml`) **already grants these verbs** on `contexts`:

```yaml
- apiGroups: ["kubocd.kubotal.io"]
  resources: ["contexts"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

So no follow-up RBAC PR is needed. Local development uses an admin kubeconfig, so writes work there
regardless.

## 8. Decisions

| # | Question | Decision |
|---|----------|----------|
| 1 | Endpoint grouping | Keep under `/api/platform-services` (symmetry with the existing read routes). **Done.** |
| 2 | Default vs per-project propagation | Accepted as-is: editing the `default` Context propagates to a project on its next `SyncFromDefault` (at deploy time). Documented; no forced re-sync for now. |
| 3 | Version validation strictness | **Hard-fail**: a version absent from the OCI registry is rejected (400). **Done** (reuses the OCI tag listing). |
| 4 | Authorization (who can edit) | **Deferred.** Should eventually be gated on the `admins` group, but the server has no auth middleware yet — tracked separately with the broader authz work. |

## 9. Implementation status

Server (this repo), handler → service → repository pattern — **implemented & tested locally**
(rebased onto the current `main`, which now includes #22's per-service `repository` override):
- ✅ `internal/repository/context_writer_repository.go` — granular catalog writers + `RetryOnConflict`; `repository` field round-trips
- ✅ `internal/service/service_service.go` — write methods + validation + OCI version check (per-service `repository` aware)
- ✅ `internal/service/package_schema_service.go` — `ListPackageTags(serviceName, repositoryOverride)` (OCI tag listing)
- ✅ `internal/api/handlers/service_handler.go` — Create/Update/Delete handlers + error mapping
- ✅ `internal/api/router/router.go` — `POST`/`PUT`/`DELETE /api/platform-services`
- ✅ `internal/service/mocks/mocks.go` — extended mocks
- ✅ RBAC — `chart/templates/rbac.yaml` (from #19) already grants `update`/`patch` on `contexts` (see §7)

Tested against a local Kind cluster: create/update/delete write the default Context; 201/200/204 on
success, 400 (validation incl. unknown OCI version)/404/409 on errors. `go build`/`vet`/`test` pass.

UI (ui#23): new `src/features/admin/catalog/` screen + `catalog-api.ts`, modeled on the Identity
page; wired once these endpoints exist — **next task**.
