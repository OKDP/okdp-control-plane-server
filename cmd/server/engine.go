package main

import (
	"fmt"

	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocd"
)

// engineConfig is the validated section of the selected deployment engine.
type engineConfig struct {
	kubocd kubocd.Config
}

// loadEngineConfig validates the section of the engine engineType names. It
// runs before anything reaches the cluster, so a wrong configuration is what
// the server reports first.
func loadEngineConfig(engineType string, lookup func(string) (string, bool)) (engineConfig, error) {
	switch engineType {
	case kubocd.Name:
		cfg, err := kubocd.LoadConfig(lookup)
		return engineConfig{kubocd: cfg}, err
	default:
		return engineConfig{}, fmt.Errorf("engine.type (ENGINE_TYPE) is %q: the supported engines are %q", engineType, kubocd.Name)
	}
}
