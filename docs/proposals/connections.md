# Design Proposal: manage external and internal connections from the Control Plane

> UI counterpart: branch `feat/connections` on OKDP/okdp-control-plane-ui
> Status: **IMPLEMENTED, behind the KuboCD connection CRDs being installed**

## 1. Problem

A project's services need to reach data that lives elsewhere: a corporate PostgreSQL, an S3
bucket. Nothing in the Control Plane lets a user declare such a resource, check that the
credentials work, or hand it to a deployed service. It has to be wired by hand.

The symmetric gap exists inside a project. If a project runs a Trino, another service of the same
project (an Airflow, say) has no guided way to consume it: someone has to look up the Kubernetes
Service and its port by hand.

Goal: one **Connections** page per project, in two sections. One holds the external resources the
user declares, the other what the project's own deployed services expose to one another. The same
external list exists at platform scope, for connections shared by every project.

## 2. Current state

> Sections 2 and 10 describe the state at the time this proposal was written, and are kept as the
> context its decisions were taken in. Three things have moved since. The kinds `Interface` and
> `ClusterInterface` are now named `Contract` and `ClusterContract`, the subsystem shipped as
> experimental with the KuboCD v0.3.2 release, and the platform-wide connection scope was dropped
> (connections are project-scoped only, nothing consumed the cluster scope). The decision not to
> depend on a KuboCD branch still holds: the server probes the cluster and degrades cleanly.

- KuboCD models this already, on the **`feat/connection2` branch** (Serge's work): `Connection`,
  `ClusterConnection`, `Interface`, `ClusterInterface`. `ConnectionSpec.outputName` separates
  connections the release controller **manages** (a deployed Trino publishing what it exposes)
  from the **unmanaged** ones a user declares. `status.parent` names the owning release.
- Those CRDs are **not in the KuboCD release the platform runs**: `api/v1alpha1/` on `main` holds
  only `release`, `config`, `context`. `ReleaseStatus` on `main` has no output/connection notion
  at all, so nothing can be read from a Release today.
- The server already has everything else needed: dynamic-client CRD repositories
  (`secret_store_repository.go`), a credentials-Secret convention (`CreateSecretStore` creates
  `<name>-credentials`), and a precedent for probing a live endpoint from the server pod
  (`POST /api/projects/:name/secret-stores/test`, which validates a Vault **token** rather than
  merely reaching the host).

## 3. Decisions

| Question | Decision | Why |
|---|---|---|
| Depend on `feat/connection2`? | **No.** Probe the cluster, degrade cleanly | The feature ships and merges on its own schedule. It activates when the CRDs land, with no code change |
| Where the connectivity test runs | Server pod, like `SecretStoreService.TestConnection` | Reuses the existing precedent. A Job in the project namespace costs RBAC, an image, cleanup and latency for a fidelity gain we do not need yet |
| A test that only reaches the host | **Rejected** | The Vault precedent asks `lookup-self` with the token whenever it has one: a wrong password must fail the test. Only Kubernetes auth, which has no token to present, falls back to `sys/health`, and says so |
| Contracts in v1 | `database-server`, `s3`, `trino`, `hive`, `iceberg-catalog` | Covers the requested cases, and the descriptor format makes a sixth contract one file |
| Where contract descriptors live | JSON embedded in the server, served by API | One descriptor drives the console form, server validation and provider matching. Cluster `Contract` schemas can be layered over them later without touching the API contract |
| Credentials | Kubernetes Secret, referenced by annotation | Keeps `spec.values` exactly the shape a `Contract` schema will validate, and reading a Connection never discloses a password |
| Scope | Project **and** platform-wide (admin) | Some connections are shared by every project |

## 4. API

```
GET    /api/contracts                                 # descriptors + crdAvailable
GET    /api/projects/{project}/connections            # external, unmanaged only
POST   /api/projects/{project}/connections
POST   /api/projects/{project}/connections/test       # never writes to the cluster
PUT    /api/projects/{project}/connections/{name}
DELETE /api/projects/{project}/connections/{name}
GET    /api/projects/{project}/connections/internal   # derived from deployed services
GET    /api/projects/{project}/connections/selectable  # satisfying a contract, for the deploy picker
GET    /api/projects/{project}/connections/{name}/consumers
```

`POST .../test` always answers **200**: a refused connection is a result to display, not a failed
request. The body carries `success`, a human-readable `message` and a `reason`
(`unreachable` | `auth-failed` | `not-found` | `timeout` | `invalid-config` | `unknown`) so the
console can tell a network problem from a credential problem.

## 5. Internal connections

Since `Release.status` carries nothing today, an internal connection is derived from:

1. the Releases of the project namespace, matched to a type through their `okdp.io/service` label.
2. the Kubernetes Services of that namespace, for the endpoint. The port is picked by the name
   the type declares, falling back to the Service's first port.

Nothing is guessed: with no Service backing the release, the endpoint is reported empty rather
than as an address that would not resolve. Headless and per-pod Services are excluded because
they address individual replicas.

Once the CRDs are installed, `ListInternal` overlays the connections the release controller owns
and drops the derived entry for the same release, so what the controller publishes wins.

## 6. Degradation

`ConnectionRepository.Available()` asks the discovery API for the `connections` resource, caching
a negative answer for 30s so installing the CRDs takes effect without a restart. Then:

- reads return an empty list, never an error, since an uninstalled optional CRD is the normal state.
- writes return `ErrConnectionsUnavailable`, which the handler maps to **501**.
- `GET /api/contracts` reports `crdAvailable: false`, and the console explains the
  situation and disables creation instead of offering a form that cannot be saved.
- internal connections are unaffected.

## 7. Credentials

`splitValues` separates the fields the descriptor marks `secret` from the rest. The secrets go to
`<name>-credentials` in the project namespace, the rest becomes `spec.values`. The Connection carries
`okdp.io/credentials-secret: <namespace>/<name>` and never the values themselves. Read paths
return only the **names** of the secret fields, so the console can show a credential as set
without being able to read it.

An edit that does not resubmit a credential keeps the stored one. The console never receives it,
so it cannot resend it. The consequence is that **testing** always demands the credential, even on
an edit: the test endpoint sees only what the form holds.

## 8. Out of scope (this proposal)

- Wiring a connection into a service's deployment parameters. That is the point of the exercise,
  but it belongs with the KuboCD packages once `Contract`s exist for these types.
- Declaring `Contract` CRDs for `postgresql`, `mysql`, `s3`. Until a package ships them, a
  created Connection does not reconcile: the object is correct, the contract it references does
  not exist yet.
- Connectivity testers beyond `database-server` and `s3`. The other contracts answer that they
  cannot be tested, and their form saves without a probe.

## 9. RBAC

Added to the server ClusterRole: `connections` and `clusterconnections` in full CRUD,
`contracts` and `clustercontracts` read-only (they are owned by the packages), and `services`
read-only for endpoint resolution. Rules on a CRD that is not installed are inert, so the chart
applies unchanged on a cluster without them.

## 10. Verification

Run against the dev sandbox, which happens to carry the `feat/connection2` CRDs:

- the five test outcomes are distinguished against a real PostgreSQL: success, wrong password,
  unknown user, missing database, closed port.
- a created Connection holds **no** password (`spec.values` has host/port/database/user/sslMode
  only) while `warehouse-credentials` does.
- the endpoint announced for an internal connection is genuinely reachable from inside the
  cluster.
- delete removes both the Connection and its Secret.
- external and internal lists never contain each other's entries.

Note for whoever re-runs this: through `kubectl port-forward` a connection appears to Postgres as
coming from `127.0.0.1`, which the official image's `pg_hba.conf` sets to `trust`. A wrong
password is then accepted and the auth test looks broken when it is not. Harden `pg_hba.conf`, or
test from inside the cluster.
