package provisioning

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
	"k8s.io/client-go/dynamic"
)

// Provider names accepted in the Context (identity.provisioning.provider).
const (
	ProviderNone     = "none"
	ProviderKubauth  = "kubauth"
	ProviderKeycloak = "keycloak"
)

type ContextConfig interface {
	GetIdentityProvisioningProvider(ctx context.Context) (string, error)
	GetKubauthNamespace(ctx context.Context) (string, error)
	GetKeycloakProvisioningConfig(ctx context.Context) (*KeycloakConfig, error)
}

type contextSelector struct {
	cfg      ContextConfig
	kubauth  *kubauthProvisioner
	keycloak *keycloakProvisioner
}

func NewContextSelector(cfg ContextConfig, client dynamic.Interface) OidcClientProvisioner {
	return &contextSelector{
		cfg:      cfg,
		kubauth:  newKubauthProvisioner(client, cfg.GetKubauthNamespace),
		keycloak: newKeycloakProvisioner(client, cfg.GetKeycloakProvisioningConfig),
	}
}

func (s *contextSelector) resolve(ctx context.Context) (OidcClientProvisioner, error) {
	provider, err := s.cfg.GetIdentityProvisioningProvider(ctx)
	if err != nil {
		return nil, err
	}
	switch provider {
	case "", ProviderNone:
		return noneProvisioner{}, nil
	case ProviderKubauth:
		return s.kubauth, nil
	case ProviderKeycloak:
		return s.keycloak, nil
	default:
		return nil, fmt.Errorf("identity.provisioning.provider %q is not supported", provider)
	}
}

func (s *contextSelector) EnsureClient(ctx context.Context, spec OidcClientSpec) error {
	p, err := s.resolve(ctx)
	if err != nil {
		return err
	}
	return p.EnsureClient(ctx, spec)
}

func (s *contextSelector) DeleteClient(ctx context.Context, name string) error {
	p, err := s.resolve(ctx)
	if err != nil {
		return err
	}
	return p.DeleteClient(ctx, name)
}

type noneProvisioner struct{}

func (noneProvisioner) EnsureClient(ctx context.Context, spec OidcClientSpec) error {
	logrus.WithField("client", spec.Name).Debug("OIDC client provisioning disabled (provider: none)")
	return nil
}

func (noneProvisioner) DeleteClient(ctx context.Context, name string) error {
	logrus.WithField("client", name).Debug("OIDC client provisioning disabled (provider: none)")
	return nil
}
