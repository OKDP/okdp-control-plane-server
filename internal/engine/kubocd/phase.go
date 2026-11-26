package kubocd

import "github.com/okdp/okdp-control-plane-server/internal/engine"

// Phase translates a KuboCD Release status.phase into the common phase. An
// empty phase means the controller has not reconciled the Release yet. The
// waiting phases (WAIT_OCI, WAIT_HREL, WAIT_DEPS...) and any word this server
// does not know count as installing.
func Phase(raw string) engine.Phase {
	switch raw {
	case "":
		return engine.PhasePending
	case "READY":
		return engine.PhaseReady
	case "ERROR", "FAILED":
		return engine.PhaseError
	case "UPGRADING":
		return engine.PhaseUpdating
	case "SUSPENDED":
		return engine.PhaseSuspended
	default:
		return engine.PhaseInstalling
	}
}
