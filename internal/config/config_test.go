package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthenticationCanBeTurnedOffOnlyByName(t *testing.T) {
	t.Setenv("AUTH_DISABLED", "true")

	cfg, err := Load()

	require.NoError(t, err)
	assert.True(t, cfg.OIDC.Disabled)
}

func TestAuthenticationIsOnUnlessTurnedOff(t *testing.T) {
	t.Setenv("AUTH_DISABLED", "")

	cfg, err := Load()

	require.NoError(t, err)
	assert.False(t, cfg.OIDC.Disabled)
}

func TestTheIssuerIsNotSetFromTheEnvironmentByDefault(t *testing.T) {
	cfg, err := Load()

	require.NoError(t, err)
	assert.Empty(t, cfg.OIDC.Issuer, "the issuer normally comes from the platform values")
}

func TestGitOpsDefaultsAndValidation(t *testing.T) {
	t.Setenv("GITOPS_REPO_URL", "")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "main", cfg.GitOps.Branch)
	assert.Equal(t, EngineFlux, cfg.GitOps.Engine)
	assert.Equal(t, "okdp-releases", cfg.GitOps.ReleasesNamespace)
	assert.Error(t, cfg.GitOps.Validate(), "no repository, no desired state")

	t.Setenv("GITOPS_REPO_URL", "https://git.example.com/okdp/deployments.git")
	t.Setenv("GITOPS_ENGINE", "ArgoCD")
	t.Setenv("GITOPS_PATH", "/gitops/")
	cfg, err = Load()
	require.NoError(t, err)
	assert.NoError(t, cfg.GitOps.Validate())
	assert.Equal(t, EngineArgoCD, cfg.GitOps.Engine)
	assert.Equal(t, "gitops", cfg.GitOps.Path)

	t.Setenv("GITOPS_ENGINE", "kubocd")
	cfg, _ = Load()
	assert.Error(t, cfg.GitOps.Validate())
}
