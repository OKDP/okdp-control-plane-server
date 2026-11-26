package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func environment(vars map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func TestTheKuboCDEngineNeedsNoConfiguration(t *testing.T) {
	cfg, err := loadEngineConfig("kubocd", environment(nil))

	require.NoError(t, err)
	assert.Equal(t, "flux", cfg.kubocd.Backend)
	assert.Equal(t, "kubernetes", cfg.kubocd.Persistence.Mode)
}

func TestAnUnknownEngineStopsTheServer(t *testing.T) {
	_, err := loadEngineConfig("helmfile", environment(nil))

	require.Error(t, err)
	assert.Equal(t, `engine.type (ENGINE_TYPE) is "helmfile": the supported engines are "kubocd"`, err.Error())
}

func TestAnInvalidEngineSectionStopsTheServer(t *testing.T) {
	_, err := loadEngineConfig("kubocd", environment(map[string]string{"ENGINE_KUBOCD_PERSISTENCE_MODE": "git"}))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ENGINE_KUBOCD_PERSISTENCE_GIT_URL")
}
