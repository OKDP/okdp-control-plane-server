# okdp-server-new

Minimal Go server for OKDP UI New, featuring a standard layered architecture and Kubernetes integration.

## Prerequisites

- **Go**: 1.24+
- **Kubernetes Cluster**: Required for Project features (local or remote).

## Setup

Projects are plain Kubernetes Namespaces labeled `okdp.io/project=<name>` —
no custom CRD or operator is required.

1. **Run Server**
   ```bash
   go run cmd/server/main.go
   ```
   The server starts on port `8093`.

## API Documentation

Swagger UI is available at:
http://localhost:8093/swagger/index.html

## Project Structure

- `cmd/server`: Entry point.
- `internal/api`: HTTP handlers and router.
- `internal/models`: Domain models.
- `internal/repository`: Data access (K8s client).
- `internal/service`: Business logic.

## Dev Setup (Keycloak + Server)

```bash
# 1. Start Keycloak
docker compose up -d

# 2. Run server
go run cmd/server/main.go
```

**Test Users** (password: `password`):
| User      | Role       | Access           |
|-----------|------------|------------------|
| useradmin | admins     | Admin space      |
| usera     | developers | Project space    |
| userb     | viewers    | Project space    |

Keycloak Admin: http://localhost:7080 (`admin` / `admin`)

