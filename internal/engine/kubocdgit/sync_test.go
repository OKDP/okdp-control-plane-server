package kubocdgit_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocd"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocdgit"
	gitopsfake "github.com/okdp/okdp-control-plane-server/internal/gitops/fake"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

// stalledSync is a sync tool whose API never answers a RequestSync. It
// records, per revision, whether the request outlived the cancellation of
// the caller and how it ended.
type stalledSync struct {
	*gitopsfake.Adapter
	mu           sync.Mutex
	cancelCaller func()
	alive        map[string]bool
	ended        map[string]error
}

func (s *stalledSync) RequestSync(ctx context.Context, revision string) error {
	s.mu.Lock()
	cancel := s.cancelCaller
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case <-ctx.Done():
	case <-time.After(20 * time.Millisecond):
		s.mu.Lock()
		s.alive[revision] = true
		s.mu.Unlock()
	}
	<-ctx.Done()
	s.mu.Lock()
	s.ended[revision] = ctx.Err()
	s.mu.Unlock()
	return ctx.Err()
}

func (s *stalledSync) result(revision string) (bool, error, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err, ended := s.ended[revision]
	return s.alive[revision], err, ended
}

func newStalledAdapter(t *testing.T) (*kubocdgit.Adapter, *stalledSync) {
	t.Helper()
	bare := newBare(t)
	store := newStore(t, bare)
	_, err := store.Bootstrap(testCtx(t))
	require.NoError(t, err)
	stalled := &stalledSync{Adapter: gitopsfake.New(), alive: map[string]bool{}, ended: map[string]error{}}
	adapter := kubocdgit.New(kubocdgit.Deps{
		Store:               store,
		Sync:                stalled,
		Platform:            &platformStub{},
		Cluster:             newCluster(),
		Config:              kubocd.Config{},
		ControlPlaneContext: repository.ContextRef{Name: "control-plane", Namespace: "okdp-system"},
		Catalog:             kubernetesCatalog,
		SyncTimeout:         time.Second,
	})
	_, err = adapter.BootstrapControlPlaneContext(testCtx(t))
	require.NoError(t, err)
	return adapter, stalled
}

func TestASlowSyncToolDelaysTheWriteByTheSyncWaitOnly(t *testing.T) {
	adapter, stalled := newStalledAdapter(t)

	started := time.Now()
	op, err := adapter.Engine().DesiredState.CreateInstance(context.Background(), jupyterhub("demo", "jh"), engine.WriteOptions{})
	elapsed := time.Since(started)

	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeAccepted, op.Outcome)
	assert.Less(t, elapsed, 500*time.Millisecond, "the write answers after the sync wait, the caller never cancelled")
	_, _, ended := stalled.result(op.DesiredRevision)
	assert.False(t, ended, "the request goes on after the write answered")
	require.Eventually(t, func() bool {
		_, _, ended := stalled.result(op.DesiredRevision)
		return ended
	}, 3*time.Second, tick)
}

func TestTheSyncRequestOutlivesACancelledCaller(t *testing.T) {
	adapter, stalled := newStalledAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stalled.mu.Lock()
	stalled.cancelCaller = cancel
	stalled.mu.Unlock()

	started := time.Now()
	op, err := adapter.Engine().DesiredState.CreateInstance(ctx, jupyterhub("demo", "jh"), engine.WriteOptions{})
	elapsed := time.Since(started)

	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeAccepted, op.Outcome, "the commit is durable whatever the sync tool does")
	assert.Less(t, elapsed, 500*time.Millisecond, "the write does not wait for the sync timeout")
	_, _, ended := stalled.result(op.DesiredRevision)
	assert.False(t, ended, "the request goes on after the write answered")

	require.Eventually(t, func() bool {
		_, _, ended := stalled.result(op.DesiredRevision)
		return ended
	}, 3*time.Second, tick)
	alive, endErr, _ := stalled.result(op.DesiredRevision)
	assert.True(t, alive, "the request survives the cancellation of the caller")
	assert.True(t, errors.Is(endErr, context.DeadlineExceeded), "bounded by the sync timeout: %v", endErr)
}
