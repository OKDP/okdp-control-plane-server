package kubocdgit_test

import (
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/enginetest"
)

func TestContract(t *testing.T) {
	enginetest.Run(t, func(t *testing.T) enginetest.Harness {
		e := newEnv(t)
		return enginetest.Harness{
			Engine:      e.engine,
			Converge:    func(t *testing.T, ref engine.InstanceRef) { e.converge(t, ref) },
			Idempotency: true,
		}
	})
}
