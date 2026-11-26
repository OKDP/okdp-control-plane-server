package fake

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/enginetest"
)

func TestFakeEngineHonoursTheContract(t *testing.T) {
	enginetest.Run(t, func(t *testing.T) enginetest.Harness {
		f := New()
		return enginetest.Harness{
			Engine:      f.Engine(),
			Converge:    func(_ *testing.T, ref engine.InstanceRef) { f.Settle(ref) },
			Idempotency: true,
		}
	})
}

var jh = engine.Instance{Ref: engine.InstanceRef{Project: "demo", Name: "jh"}, Service: "jupyterhub", Version: "1.0.0"}

func TestAnAcceptedWriteStaysPendingUntilTheEngineActs(t *testing.T) {
	f := New()
	ctx := context.Background()

	op, err := f.CreateInstance(ctx, jh, engine.WriteOptions{})
	require.NoError(t, err)
	require.NotNil(t, op.Instance)
	assert.Equal(t, engine.PhasePending, op.Instance.Status.Phase)
	assert.Len(t, op.DesiredRevision, 40, "a commit-like revision")
	assert.Empty(t, op.Instance.Status.AppliedRevision)

	for _, want := range []engine.Phase{engine.PhasePending, engine.PhaseInstalling, engine.PhaseReady} {
		view, err := f.ObserveInstance(ctx, jh.Ref)
		require.NoError(t, err)
		assert.Equal(t, want, view.Status.Phase)
		f.Advance(jh.Ref)
	}

	view, err := f.ObserveInstance(ctx, jh.Ref)
	require.NoError(t, err)
	assert.Equal(t, op.DesiredRevision, view.Status.AppliedRevision, "Ready proves the revision applied")
	assert.Empty(t, view.Status.PublishedRevision, "the fake has no build step")
}

func TestAnUpdateGoesThroughUpdating(t *testing.T) {
	f := New()
	ctx := context.Background()
	_, err := f.CreateInstance(ctx, jh, engine.WriteOptions{})
	require.NoError(t, err)
	f.Settle(jh.Ref)

	_, err = f.UpdateInstance(ctx, jh, engine.WriteOptions{})
	require.NoError(t, err)
	f.Advance(jh.Ref)

	view, err := f.ObserveInstance(ctx, jh.Ref)
	require.NoError(t, err)
	assert.Equal(t, engine.PhaseUpdating, view.Status.Phase)
}

func TestARejectedWriteStoresNothing(t *testing.T) {
	f := New()
	f.RejectNext(engine.Problem{Code: engine.CodeInvalidParameters, Message: "replicas must be positive"})

	op, err := f.CreateInstance(context.Background(), jh, engine.WriteOptions{})

	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeRejected, op.Outcome)
	assert.Empty(t, op.DesiredRevision)
	_, err = f.ObserveInstance(context.Background(), jh.Ref)
	assert.ErrorIs(t, err, engine.ErrNotFound)
}

func TestAnUnknownOutcomeMayHaveBeenStored(t *testing.T) {
	f := New()
	f.UnknownNext()

	op, err := f.CreateInstance(context.Background(), jh, engine.WriteOptions{})

	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeUnknown, op.Outcome)
	assert.Empty(t, op.DesiredRevision)
	assert.Nil(t, op.Instance)
	view, err := f.ObserveInstance(context.Background(), jh.Ref)
	require.NoError(t, err, "reading again tells what happened")
	assert.Equal(t, engine.PhasePending, view.Status.Phase)
}

func TestASlowWatcherIsClosedRatherThanBlockingTheEngine(t *testing.T) {
	f := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := f.WatchInstances(ctx, "demo")
	require.NoError(t, err)

	for i := 0; i <= watchBuffer; i++ {
		_, err := f.UpdateInstance(ctx, jh, engine.WriteOptions{})
		if i == 0 {
			require.ErrorIs(t, err, engine.ErrNotFound)
			_, err = f.CreateInstance(ctx, jh, engine.WriteOptions{})
		}
		require.NoError(t, err)
	}

	received := 0
	for range events {
		received++
	}
	assert.Equal(t, watchBuffer, received)
	assert.Zero(t, f.WatcherCount("demo"), "a closed watch is forgotten even though its context lives on")
}

func TestEveryReaderGetsItsOwnCopyOfAnInstance(t *testing.T) {
	f := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := f.WatchInstances(ctx, "demo")
	require.NoError(t, err)
	second, err := f.WatchInstances(ctx, "demo")
	require.NoError(t, err)
	in := jh
	in.Parameters = map[string]any{"replicas": float64(1)}

	op, err := f.CreateInstance(ctx, in, engine.WriteOptions{IdempotencyKey: "k"})
	require.NoError(t, err)
	op.Instance.Parameters["replicas"] = float64(9)
	event := <-first
	event.Instance.Parameters["replicas"] = float64(8)

	assert.Equal(t, float64(1), (<-second).Instance.Parameters["replicas"])
	replay, err := f.CreateInstance(ctx, in, engine.WriteOptions{IdempotencyKey: "k"})
	require.NoError(t, err)
	assert.Equal(t, float64(1), replay.Instance.Parameters["replicas"])
	stored, _, err := f.GetInstance(ctx, jh.Ref)
	require.NoError(t, err)
	assert.Equal(t, float64(1), stored.Parameters["replicas"])
}

func TestCapabilitiesDescribeAGitEngine(t *testing.T) {
	info := New().Engine().Info
	assert.Equal(t, engine.PersistenceGit, info.Persistence.Mode)
}
