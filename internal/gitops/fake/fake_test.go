package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
)

func TestRequestSyncRecordsRevisionsInOrder(t *testing.T) {
	a := New()
	require.NoError(t, a.RequestSync(context.Background(), "a"))
	require.NoError(t, a.RequestSync(context.Background(), "b"))
	assert.Equal(t, []string{"a", "b"}, a.Requests())
}

func TestFailedRequestSyncRecordsNothing(t *testing.T) {
	a := New()
	a.RequestErr = errors.New("unreachable")
	assert.ErrorIs(t, a.RequestSync(context.Background(), "a"), a.RequestErr)
	assert.Empty(t, a.Requests())
}

func TestWatchSyncStartsWithTheCurrentStateAndKeepsTheLatest(t *testing.T) {
	a := New()
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := a.WatchSync(ctx)
	require.NoError(t, err)

	first := <-ch
	assert.Equal(t, gitops.SyncUnknown, first.Phase)

	a.SetState(gitops.SyncState{Phase: gitops.SyncProgressing, FetchedRevision: "r1"})
	a.SetState(gitops.SyncState{Phase: gitops.SyncReady, AppliedRevision: "r2"})
	latest := <-ch
	assert.Equal(t, gitops.SyncReady, latest.Phase)
	assert.Equal(t, "r2", latest.AppliedRevision)
	assert.False(t, latest.ObservedAt.IsZero())

	observed, err := a.ObserveSync(context.Background())
	require.NoError(t, err)
	assert.Equal(t, latest, observed)

	cancel()
	for range ch {
	}
}

func TestAFailedSyncCarriesTheAttemptedRevision(t *testing.T) {
	a := New()
	a.SetState(gitops.SyncState{Phase: gitops.SyncFailed, AppliedRevision: "r1", AttemptedRevision: "r2", Message: "apply failed"})
	st, err := a.ObserveSync(context.Background())
	require.NoError(t, err)
	assert.Equal(t, gitops.SyncFailed, st.Phase)
	assert.Equal(t, "r1", st.AppliedRevision)
	assert.Equal(t, "r2", st.AttemptedRevision)

	a.SetState(gitops.SyncState{Phase: gitops.SyncSuspended, AppliedRevision: "r1"})
	st, err = a.ObserveSync(context.Background())
	require.NoError(t, err)
	assert.Equal(t, gitops.SyncSuspended, st.Phase)
	assert.Empty(t, st.AttemptedRevision)
}
