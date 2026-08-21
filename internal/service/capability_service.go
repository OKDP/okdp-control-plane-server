package service

import (
	"context"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
)

// Identity providers accepted in the Context (identity.provider).
const (
	IdentityProviderExternal = "external"
	IdentityProviderKubauth  = "kubauth"
)

// CapabilityService derives the platform capabilities from the Context, at
// request time so configuration changes apply without restarting the server.
type CapabilityService interface {
	// GetCapabilities returns the capabilities advertised to the UI.
	GetCapabilities(ctx context.Context) (*models.Capabilities, error)

	// IdentityAPIEnabled reports whether the kubauth-specific user/group
	// management API is available (identity.provider == kubauth).
	IdentityAPIEnabled(ctx context.Context) (bool, error)
}

type DefaultCapabilityService struct {
	contextRepo repository.ContextRepository
	// Reports whether the kubauth CRDs are served. The identity routes rest on
	// the same answer, so advertising the capability without it would offer a
	// section whose every call comes back 501.
	identityCRDsInstalled func(ctx context.Context) bool
}

func NewDefaultCapabilityService(contextRepo repository.ContextRepository, identityCRDsInstalled func(ctx context.Context) bool) *DefaultCapabilityService {
	return &DefaultCapabilityService{contextRepo: contextRepo, identityCRDsInstalled: identityCRDsInstalled}
}

func (s *DefaultCapabilityService) identityCRDs(ctx context.Context) bool {
	return s.identityCRDsInstalled != nil && s.identityCRDsInstalled(ctx)
}

func (s *DefaultCapabilityService) GetCapabilities(ctx context.Context) (*models.Capabilities, error) {
	identityProvider, err := s.contextRepo.GetIdentityProvider(ctx)
	if err != nil {
		return nil, err
	}
	if identityProvider == "" {
		identityProvider = IdentityProviderExternal
	}

	oidc, err := s.contextRepo.GetIdentityOidcConfig(ctx)
	if err != nil {
		return nil, err
	}

	provisioningProvider, err := s.contextRepo.GetIdentityProvisioningProvider(ctx)
	if err != nil {
		return nil, err
	}
	if provisioningProvider == "" {
		provisioningProvider = provisioning.ProviderNone
	}

	return &models.Capabilities{
		Identity: models.IdentityCapability{
			Provider:       identityProvider,
			UserManagement: identityProvider == IdentityProviderKubauth && s.identityCRDs(ctx),
			Oidc:           oidc,
		},
		OidcProvisioning: models.OidcProvisioningCapability{
			Provider: provisioningProvider,
		},
	}, nil
}

func (s *DefaultCapabilityService) IdentityAPIEnabled(ctx context.Context) (bool, error) {
	provider, err := s.contextRepo.GetIdentityProvider(ctx)
	if err != nil {
		return false, err
	}
	return provider == IdentityProviderKubauth, nil
}
