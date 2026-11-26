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

func TestTheEngineIsKuboCDUnlessChosen(t *testing.T) {
	for _, value := range []string{"", "  "} {
		t.Setenv("ENGINE_TYPE", value)
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, "kubocd", cfg.EngineType)
	}

	t.Setenv("ENGINE_TYPE", "helmfile")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "helmfile", cfg.EngineType, "the core passes the choice on, the engine registry refuses it")
}

func TestContextDefaultsSeparateControlPlaneAndPlatform(t *testing.T) {
	for _, key := range []string{
		"CONTROL_PLANE_CONTEXT_NAME", "CONTROL_PLANE_CONTEXT_NAMESPACE",
		"PLATFORM_CONTEXT_NAME", "PLATFORM_CONTEXT_NAMESPACE",
		"CONTEXT_NAME", "CONTEXT_NAMESPACE", "POD_NAMESPACE",
	} {
		t.Setenv(key, "")
	}

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "control-plane", cfg.ControlPlaneContextName)
	assert.Equal(t, "okdp-system", cfg.ControlPlaneContextNamespace)
	assert.Equal(t, "platform", cfg.PlatformContextName)
	assert.Equal(t, "okdp-system", cfg.PlatformContextNamespace)
}

func TestContextVariablesAreTrimmedAndBlankCountsAsUnset(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "  runtime-ns  ")
	t.Setenv("CONTROL_PLANE_CONTEXT_NAME", "  ")
	t.Setenv("CONTROL_PLANE_CONTEXT_NAMESPACE", "\t")
	t.Setenv("PLATFORM_CONTEXT_NAME", "   ")
	t.Setenv("PLATFORM_CONTEXT_NAMESPACE", " ")
	t.Setenv("CONTEXT_NAME", "  ")
	t.Setenv("CONTEXT_NAMESPACE", "  ")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "control-plane", cfg.ControlPlaneContextName)
	assert.Equal(t, "runtime-ns", cfg.ControlPlaneContextNamespace)
	assert.Equal(t, "platform", cfg.PlatformContextName)
	assert.Equal(t, "runtime-ns", cfg.PlatformContextNamespace)

	t.Setenv("CONTROL_PLANE_CONTEXT_NAME", " cp-ctx ")
	t.Setenv("CONTROL_PLANE_CONTEXT_NAMESPACE", " cp-ns ")
	t.Setenv("PLATFORM_CONTEXT_NAME", " plat-ctx ")
	t.Setenv("PLATFORM_CONTEXT_NAMESPACE", " ")

	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "cp-ctx", cfg.ControlPlaneContextName)
	assert.Equal(t, "cp-ns", cfg.ControlPlaneContextNamespace)
	assert.Equal(t, "plat-ctx", cfg.PlatformContextName)
	assert.Equal(t, "cp-ns", cfg.PlatformContextNamespace, "platform namespace defaults to the control plane namespace")
}

func TestLegacyContextVariablesKeepMainUpgradesWorking(t *testing.T) {
	t.Setenv("CONTROL_PLANE_CONTEXT_NAME", "")
	t.Setenv("CONTROL_PLANE_CONTEXT_NAMESPACE", "")
	t.Setenv("PLATFORM_CONTEXT_NAME", "")
	t.Setenv("PLATFORM_CONTEXT_NAMESPACE", "")
	t.Setenv("CONTEXT_NAME", " custom-platform ")
	t.Setenv("CONTEXT_NAMESPACE", " legacy-ns ")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "control-plane", cfg.ControlPlaneContextName)
	assert.Equal(t, "legacy-ns", cfg.ControlPlaneContextNamespace)
	assert.Equal(t, "custom-platform", cfg.PlatformContextName)
	assert.Equal(t, "legacy-ns", cfg.PlatformContextNamespace)

	// Explicit new variables take precedence over legacy ones.
	t.Setenv("CONTROL_PLANE_CONTEXT_NAME", "cp")
	t.Setenv("CONTROL_PLANE_CONTEXT_NAMESPACE", "cp-ns")
	t.Setenv("PLATFORM_CONTEXT_NAME", "plat")
	t.Setenv("PLATFORM_CONTEXT_NAMESPACE", "plat-ns")

	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "cp", cfg.ControlPlaneContextName)
	assert.Equal(t, "cp-ns", cfg.ControlPlaneContextNamespace)
	assert.Equal(t, "plat", cfg.PlatformContextName)
	assert.Equal(t, "plat-ns", cfg.PlatformContextNamespace)
}
