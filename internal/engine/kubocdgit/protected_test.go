package kubocdgit

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
)

func TestReleaseManifestSetsProtectedFalseForGitOpsPrune(t *testing.T) {
	a := &Adapter{}
	manifest := a.releaseManifest(engine.Instance{
		Ref:     engine.InstanceRef{Project: "demo", Name: "trino"},
		Service: "trino",
		Version: "1.0.0",
	}, "demo-trino", "quay.io/okdp/platform-packages")

	spec, ok := manifest["spec"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, false, spec["protected"])
	assert.Equal(t, false, spec["createNamespace"])
}
