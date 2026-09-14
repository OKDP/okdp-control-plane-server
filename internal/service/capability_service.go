package service

import (
	"context"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
)

// Identity providers reported in /api/capabilities (identity.provider).
const (
	// Users are managed outside the console (in Keycloak directly).
	IdentityProviderExternal = "external"
	// The console manages users and groups through the Keycloak Admin API.
	IdentityProviderKeycloak = "keycloak"
)

// CapabilityService derives the platform capabilities from the platform values, at
// request time so configuration changes apply without restarting the server.
type CapabilityService interface {
	// GetCapabilities returns the capabilities advertised to the UI.
	GetCapabilities(ctx context.Context) (*models.Capabilities, error)
}

type DefaultCapabilityService struct {
	contextRepo repository.PlatformRepository
	// Reports whether the identity API is backed (Keycloak admin credentials
	// and realm). The identity routes rest on the same answer, so the section
	// is never advertised while every call would come back 501.
	userManagement func(ctx context.Context) bool
}

func NewDefaultCapabilityService(contextRepo repository.PlatformRepository, userManagement func(ctx context.Context) bool) *DefaultCapabilityService {
	return &DefaultCapabilityService{contextRepo: contextRepo, userManagement: userManagement}
}

func (s *DefaultCapabilityService) GetCapabilities(ctx context.Context) (*models.Capabilities, error) {
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

	userManagement := s.userManagement != nil && s.userManagement(ctx)
	identityProvider := IdentityProviderExternal
	if userManagement {
		identityProvider = IdentityProviderKeycloak
	}

	return &models.Capabilities{
		Identity: models.IdentityCapability{
			Provider:       identityProvider,
			UserManagement: userManagement,
			Oidc:           oidc,
		},
		OidcProvisioning: models.OidcProvisioningCapability{
			Provider: provisioningProvider,
		},
	}, nil
}
