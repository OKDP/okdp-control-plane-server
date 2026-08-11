package service

import (
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A release name is also the start of the name of any release extending it, so
// deleting `jupyter` in a project that also runs `jupyter-ds` must leave the
// home volumes of the other instance's users alone.
func TestOwnsByNameSparesTheNeighboursObjects(t *testing.T) {
	others := []string{"demo-jupyter-ds", "demo-trino"}

	// The home volume and the running server of a user of the *other* instance.
	assert.False(t, ownsByName("demo-jupyter-ds-claim-alice", "demo-jupyter", others),
		"a home volume of the jupyter-ds instance must survive deleting jupyter")
	assert.False(t, ownsByName("demo-jupyter-ds-alice", "demo-jupyter", others))

	// An object carrying the neighbour's own name, as a chart naming its volume
	// after the release does.
	assert.False(t, ownsByName("demo-jupyter-ds", "demo-jupyter", others),
		"an object named exactly like the jupyter-ds release belongs to it")

	// The objects of the instance actually being deleted.
	assert.True(t, ownsByName("demo-jupyter-claim-bob", "demo-jupyter", others))
	assert.True(t, ownsByName("demo-jupyter-bob", "demo-jupyter", others))
}

func TestOwnsByNameIgnoresWhatIsNotItsOwn(t *testing.T) {
	others := []string{"demo-trino"}

	// A different release entirely.
	assert.False(t, ownsByName("demo-trino-worker-0", "demo-jupyter", others))
	// The release's own object, no separator: `demo-jupyterhub` is another name,
	// not a child of `demo-jupyter`.
	assert.False(t, ownsByName("demo-jupyterhub", "demo-jupyter", others))
	// The release object itself carries no suffix.
	assert.False(t, ownsByName("demo-jupyter", "demo-jupyter", others))
}

// A neighbour whose name is shorter cannot own an object that matches the
// longer prefix, so it must not block the cleanup.
func TestOwnsByNameIgnoresShorterNeighbours(t *testing.T) {
	assert.True(t, ownsByName("demo-jupyter-ds-claim-alice", "demo-jupyter-ds", []string{"demo-jupyter"}))
}

// A connectionRef with no kind resolves by looking on both sides, so the
// controller records a Connection and a ClusterConnection candidate per name.
// Listing them raw showed "demo-db (ClusterConnection) waiting" beside the
// Connection that had resolved, for a ClusterConnection that never existed.
func TestBoundConnectionsGroupsTheCandidatesOfOneName(t *testing.T) {
	release := &crd.Release{
		Status: crd.ReleaseStatus{
			WatchedInputConnections: []crd.InputConnectionReference{
				{Kind: "ClusterConnection", Name: "demo-db"},
				{Kind: "Connection", Name: "demo-db", Namespace: "demo"},
				{Kind: "ClusterConnection", Name: "shared-cache"},
				{Kind: "Connection", Name: "shared-cache", Namespace: "demo"},
			},
			EffectiveInputConnections: []crd.InputConnectionReference{
				{Kind: "Connection", Name: "demo-db", Namespace: "demo"},
			},
		},
	}

	connections := boundConnections(release)

	require.Len(t, connections, 2, "one entry per name, not one per candidate")
	assert.Equal(t, "demo-db", connections[0].Name)
	assert.True(t, connections[0].Resolved)
	assert.Equal(t, "Connection", connections[0].Kind, "the candidate that actually resolved")
	// Nothing of that name resolved anywhere: the service is waiting for it.
	assert.Equal(t, "shared-cache", connections[1].Name)
	assert.False(t, connections[1].Resolved)
}
