# okdp-server-new

Minimal Go server for OKDP UI New, featuring a standard layered architecture and Kubernetes integration.

## Prerequisites

- **Go**: 1.24+
- **Kubernetes Cluster**: Required for Project features (local or remote).

## Development

Projects are plain Kubernetes Namespaces labeled `okdp.io/project=<name>`, with no
custom CRD or operator required.

Two ways to develop, both against the **OKDP dev-sandbox** running on the host (it
provides the cluster, kubauth, DNS and the CA certificate). Set it up via the
`okdp-control-plane-dev-sandbox` README (cluster, DNS resolver, CA trust).

**Option A: directly on your machine.** Install Go, kubectl, kubocd, air, swag,
golangci-lint, delve.

```bash
export KUBECONFIG=~/.kube/okdp-dev-config
make dev                 # hot-reload on :8093
# or, without air: go run ./cmd/server
```

**Option B: devcontainer (only Docker needed).** Open the repo and "Reopen in
Container", or run `devcontainer up`. The image ships every tool, derives a
container-reachable kubeconfig on start, and publishes port 8093.

```bash
make dev
```

`make help` lists `build`, `test`, `lint` and `swagger` (same in both modes).

## Deploy with the Helm chart

The server runs **in-cluster** from the image published by CI, deployed with the
bundled chart (`chart/`):

```bash
helm install okdp-server ./chart -n okdp-system
```

For local development you can still run `go run cmd/server/main.go`.

## API Documentation

Swagger UI is available at:
http://localhost:8093/swagger/index.html

## Project Structure

- `cmd/server`: Entry point.
- `internal/api`: HTTP handlers and router.
- `internal/models`: Domain models.
- `internal/repository`: Data access (K8s client).
- `internal/service`: Business logic.

