# Local Development

## Prerequisites

* Docker installed and running
* A Kubernetes cluster configured in `~/.kube/config`

No local Go installation is required.

## Cluster requirements

The server requires access to a Kubernetes cluster running the minimal OKDP platform components:

* KuboCD
* Kubauth
* Required CRDs and namespaces

The cluster can be provided by any compatible environment:

* Kind
* K3s
* K8s
* Minikube
* Remote Kubernetes cluster
* Any other Kubernetes distribution

As long as a valid kubeconfig is available in `~/.kube/config`, the server can connect to the cluster.

## Start the development environment

Open the repository in VS Code and select:

```text
Reopen in Container
```

The devcontainer installs all required development tools automatically.

## Run the server

```bash
make dev
```

The API starts on port `8093` with hot reload enabled.

## Available commands

```bash
make dev           Start server with hot reload
make build         Build the server binary
make test          Run tests
make test-verbose  Run tests with verbose output
make lint          Run golangci-lint
make swagger       Regenerate Swagger documentation
```

## Included tools

| Tool          | Purpose                   |
| ------------- | ------------------------- |
| Go            | Build and run the server  |
| kubectl       | Interact with the cluster |
| kubocd        | KuboCD CLI — **version must match the cluster controller** |
| air           | Hot reload                |
| swag          | Swagger generation        |
| golangci-lint | Static analysis           |
| delve         | Debugging                 |

## Configuration

The server uses the kubeconfig mounted from the host:

```text
~/.kube/config
```

Default values:

```bash
CONTEXT_NAME=default
CONTEXT_NAMESPACE=kubocd-system
PLATFORM_NAMESPACE=okdp-system
```

These values only need to be changed when connecting to a custom environment.
