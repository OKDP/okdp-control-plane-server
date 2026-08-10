package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Deleting a service used to delete every pod and volume whose name started
// with the release name, which is also the start of the names of any release
// whose name extends it. Deleting `jupyter` in a project that also runs
// `jupyter-ds` therefore destroyed the home volumes of the other instance's
// users, silently, and the confirmation dialog only mentioned the service being
// deleted.
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
