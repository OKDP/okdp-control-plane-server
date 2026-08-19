package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

// Only the three getters the capability service reads are implemented; the
// embedded interface makes any other call panic rather than pass silently.
type stubContextRepo struct {
	repository.ContextRepository
	provider     string
	provisioning string
}

func (s stubContextRepo) GetIdentityProvider(context.Context) (string, error) {
	return s.provider, nil
}

func (s stubContextRepo) GetIdentityOidcConfig(context.Context) (*models.IdentityOidcConfig, error) {
	return nil, nil
}

func (s stubContextRepo) GetIdentityProvisioningProvider(context.Context) (string, error) {
	return s.provisioning, nil
}

// The Context names kubauth as the provider, but the cluster does not serve its
// CRDs. The identity routes answer 501 in that state, so the capability must
// not advertise a section whose every call fails.
func TestUserManagementNeedsTheKubauthCRDs(t *testing.T) {
	repo := stubContextRepo{provider: IdentityProviderKubauth}

	withoutCRDs := NewDefaultCapabilityService(repo, func(context.Context) bool { return false })
	capabilities, err := withoutCRDs.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.False(t, capabilities.Identity.UserManagement, "the CRDs are absent, the section cannot work")

	withCRDs := NewDefaultCapabilityService(repo, func(context.Context) bool { return true })
	capabilities, err = withCRDs.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.True(t, capabilities.Identity.UserManagement)
}

// An external provider never exposes the kubauth API, CRDs or not.
func TestUserManagementIsOffForAnExternalProvider(t *testing.T) {
	svc := NewDefaultCapabilityService(stubContextRepo{}, func(context.Context) bool { return true })

	capabilities, err := svc.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.Equal(t, IdentityProviderExternal, capabilities.Identity.Provider)
	assert.False(t, capabilities.Identity.UserManagement)
}
