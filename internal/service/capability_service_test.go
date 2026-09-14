package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
)

// Only the getters the capability service reads are implemented; the
// embedded interface makes any other call panic rather than pass silently.
type stubContextRepo struct {
	repository.PlatformRepository
	oidc         *models.IdentityOidcConfig
	provisioning string
}

func (s stubContextRepo) GetIdentityOidcConfig(context.Context) (*models.IdentityOidcConfig, error) {
	return s.oidc, nil
}

func (s stubContextRepo) GetIdentityProvisioningProvider(context.Context) (string, error) {
	return s.provisioning, nil
}

// The console reads its OIDC client from the capabilities: it must be passed
// through untouched.
func TestCapabilitiesCarryTheConsoleOidcClient(t *testing.T) {
	oidc := &models.IdentityOidcConfig{Authority: "https://keycloak.example/realms/okdp", ClientID: "okdp-console"}
	svc := NewDefaultCapabilityService(stubContextRepo{oidc: oidc, provisioning: provisioning.ProviderKeycloak}, nil)

	capabilities, err := svc.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.Equal(t, oidc, capabilities.Identity.Oidc)
	assert.Equal(t, provisioning.ProviderKeycloak, capabilities.OidcProvisioning.Provider)
}

// An unset provisioning backend is advertised as "none", not as an empty string.
func TestProvisioningDefaultsToNone(t *testing.T) {
	svc := NewDefaultCapabilityService(stubContextRepo{}, nil)

	capabilities, err := svc.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.Nil(t, capabilities.Identity.Oidc)
	assert.Equal(t, provisioning.ProviderNone, capabilities.OidcProvisioning.Provider)
}

// User management is advertised on exactly the condition the identity routes
// are guarded on: Keycloak admin credentials and a realm. Without them the
// section would offer screens whose every call answers 501.
func TestUserManagementFollowsTheKeycloakAdminClient(t *testing.T) {
	without := NewDefaultCapabilityService(stubContextRepo{}, func(context.Context) bool { return false })
	capabilities, err := without.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.False(t, capabilities.Identity.UserManagement)
	assert.Equal(t, IdentityProviderExternal, capabilities.Identity.Provider)

	with := NewDefaultCapabilityService(stubContextRepo{}, func(context.Context) bool { return true })
	capabilities, err = with.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.True(t, capabilities.Identity.UserManagement)
	assert.Equal(t, IdentityProviderKeycloak, capabilities.Identity.Provider)
}

// No probe at all (nil) is not a reason to advertise the section.
func TestUserManagementIsOffWithoutAProbe(t *testing.T) {
	svc := NewDefaultCapabilityService(stubContextRepo{}, nil)

	capabilities, err := svc.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.False(t, capabilities.Identity.UserManagement)
	assert.Equal(t, IdentityProviderExternal, capabilities.Identity.Provider)
}
