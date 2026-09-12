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
	assert.Empty(t, cfg.OIDC.Issuer, "the issuer normally comes from the platform Context")
}
