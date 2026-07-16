package service

import (
	"context"

	"github.com/okdp/okdp-server-new/internal/models"
	"github.com/okdp/okdp-server-new/internal/repository"
	"github.com/okdp/okdp-server-new/internal/repository/provisioning"
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
}

func NewDefaultCapabilityService(contextRepo repository.ContextRepository) *DefaultCapabilityService {
	return &DefaultCapabilityService{contextRepo: contextRepo}
}

func (s *DefaultCapabilityService) GetCapabilities(ctx context.Context) (*models.Capabilities, error) {
	identityProvider, err := s.contextRepo.GetIdentityProvider(ctx)
	if err != nil {
		return nil, err
	}
	if identityProvider == "" {
		identityProvider = IdentityProviderExternal
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
			UserManagement: identityProvider == IdentityProviderKubauth,
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
