package provisioning

import (
	"context"
)

type OidcClientSpec struct {
	Name         string
	RedirectURIs []string
}

type OidcClientProvisioner interface {
	EnsureClient(ctx context.Context, spec OidcClientSpec) error
	DeleteClient(ctx context.Context, name string) error
}
