// Package enginetest is the contract every deployment engine passes. An
// engine's test calls Run with a harness that builds a fresh engine and can
// drive it to convergence.
package enginetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
)

// Harness gives the contract a fresh engine and a way to drive it.
type Harness struct {
	Engine engine.Engine
	// Converge makes the engine finish whatever it has to do for an instance:
	// reach a terminal phase, or disappear after a deletion.
	Converge func(t *testing.T, ref engine.InstanceRef)
	// Idempotency reports that the engine honours WriteOptions.IdempotencyKey.
	Idempotency bool
}

// eventTimeout bounds every wait on a watch channel.
const eventTimeout = 5 * time.Second

func sample(project, name string) engine.Instance {
	return engine.Instance{
		Ref:        engine.InstanceRef{Project: project, Name: name},
		Service:    "jupyterhub",
		Version:    "1.0.0",
		Parameters: map[string]any{"replicas": float64(1)},
	}
}

// accepted asserts a write was accepted: accepted(t)(engine.CreateInstance(...)).
// sameJSON compares two values as JSON: a stored 1 may come back as an
// integer or a float depending on the engine.
func sameJSON(t *testing.T, want, got any) {
	t.Helper()
	wantJSON, err := json.Marshal(want)
	require.NoError(t, err)
	gotJSON, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, string(wantJSON), string(gotJSON))
}

func accepted(t *testing.T) func(engine.Operation, error) engine.Operation {
	return func(op engine.Operation, err error) engine.Operation {
		t.Helper()
		require.NoError(t, err)
		require.Equal(t, engine.OutcomeAccepted, op.Outcome, "problems: %v", op.Problems)
		return op
	}
}

func rejectedWith(t *testing.T, op engine.Operation, err error, code string) {
	t.Helper()
	require.NoError(t, err)
	require.Equal(t, engine.OutcomeRejected, op.Outcome)
	require.NotEmpty(t, op.Problems)
	assert.Equal(t, code, op.Problems[0].Code)
	assert.NotEmpty(t, op.Problems[0].Message)
}

func next(t *testing.T, events <-chan engine.InstanceEvent) engine.InstanceEvent {
	t.Helper()
	select {
	case event, ok := <-events:
		require.True(t, ok, "the watch closed")
		return event
	case <-time.After(eventTimeout):
		t.Fatal("no event within the timeout")
		return engine.InstanceEvent{}
	}
}

// Run checks an engine against the contract.
func Run(t *testing.T, newHarness func(t *testing.T) Harness) {
	ctx := context.Background()

	t.Run("a create is accepted with a revision and starts short of Ready", func(t *testing.T) {
		h := newHarness(t)
		in := sample("demo", "jh")

		op := accepted(t)(h.Engine.DesiredState.CreateInstance(ctx, in, engine.WriteOptions{}))
		require.NotEmpty(t, op.DesiredRevision)

		desired, revision, err := h.Engine.DesiredState.GetInstance(ctx, in.Ref)
		require.NoError(t, err)
		assert.Equal(t, op.DesiredRevision, revision)
		assert.Equal(t, in.Service, desired.Service)
		assert.Equal(t, in.Version, desired.Version)
		sameJSON(t, in.Parameters, desired.Parameters)

		view, err := h.Engine.Observer.ObserveInstance(ctx, in.Ref)
		require.NoError(t, err)
		assert.Equal(t, in.Ref, view.Ref)
		assert.NotEmpty(t, view.EngineName)
		assert.NotEqual(t, engine.PhaseReady, view.Status.Phase)
		assert.NotEqual(t, engine.PhaseUnknown, view.Status.Phase, "an accepted write is Pending, not Unknown")
		assert.Equal(t, op.DesiredRevision, view.Status.DesiredRevision)
	})

	t.Run("an instance converges to Ready", func(t *testing.T) {
		h := newHarness(t)
		in := sample("demo", "jh")
		accepted(t)(h.Engine.DesiredState.CreateInstance(ctx, in, engine.WriteOptions{}))

		h.Converge(t, in.Ref)

		view, err := h.Engine.Observer.ObserveInstance(ctx, in.Ref)
		require.NoError(t, err)
		assert.Equal(t, engine.PhaseReady, view.Status.Phase)
	})

	t.Run("a second create of the same instance is rejected and changes nothing", func(t *testing.T) {
		h := newHarness(t)
		in := sample("demo", "jh")
		first := accepted(t)(h.Engine.DesiredState.CreateInstance(ctx, in, engine.WriteOptions{}))

		other := in
		other.Version = "2.0.0"
		op, err := h.Engine.DesiredState.CreateInstance(ctx, other, engine.WriteOptions{})
		rejectedWith(t, op, err, engine.CodeInstanceExists)

		desired, revision, err := h.Engine.DesiredState.GetInstance(ctx, in.Ref)
		require.NoError(t, err)
		assert.Equal(t, "1.0.0", desired.Version)
		assert.Equal(t, first.DesiredRevision, revision)
	})

	t.Run("an update replaces the desired state with a new revision", func(t *testing.T) {
		h := newHarness(t)
		in := sample("demo", "jh")
		first := accepted(t)(h.Engine.DesiredState.CreateInstance(ctx, in, engine.WriteOptions{}))
		h.Converge(t, in.Ref)

		updated := in
		updated.Version = "1.1.0"
		updated.Parameters = map[string]any{"profile": "large"}
		op := accepted(t)(h.Engine.DesiredState.UpdateInstance(ctx, updated, engine.WriteOptions{ExpectedRevision: first.DesiredRevision}))
		assert.NotEmpty(t, op.DesiredRevision)
		assert.NotEqual(t, first.DesiredRevision, op.DesiredRevision)

		desired, revision, err := h.Engine.DesiredState.GetInstance(ctx, in.Ref)
		require.NoError(t, err)
		assert.Equal(t, "1.1.0", desired.Version)
		assert.Equal(t, map[string]any{"profile": "large"}, desired.Parameters, "the parameters are replaced, not merged")
		assert.Equal(t, op.DesiredRevision, revision)
	})

	t.Run("an update against a stale revision is rejected and changes nothing", func(t *testing.T) {
		h := newHarness(t)
		in := sample("demo", "jh")
		first := accepted(t)(h.Engine.DesiredState.CreateInstance(ctx, in, engine.WriteOptions{}))
		changed := in
		changed.Version = "1.1.0"
		second := accepted(t)(h.Engine.DesiredState.UpdateInstance(ctx, changed, engine.WriteOptions{}))

		late := in
		late.Version = "9.9.9"
		op, err := h.Engine.DesiredState.UpdateInstance(ctx, late, engine.WriteOptions{ExpectedRevision: first.DesiredRevision})
		rejectedWith(t, op, err, engine.CodeConflict)

		desired, revision, err := h.Engine.DesiredState.GetInstance(ctx, in.Ref)
		require.NoError(t, err)
		assert.Equal(t, "1.1.0", desired.Version)
		assert.Equal(t, second.DesiredRevision, revision)
	})

	t.Run("a deleted instance goes away", func(t *testing.T) {
		h := newHarness(t)
		in := sample("demo", "jh")
		accepted(t)(h.Engine.DesiredState.CreateInstance(ctx, in, engine.WriteOptions{}))
		h.Converge(t, in.Ref)

		op := accepted(t)(h.Engine.DesiredState.DeleteInstance(ctx, in.Ref, engine.WriteOptions{}))
		if op.Instance != nil {
			assert.NotEmpty(t, op.Instance.EngineName)
		}
		// Until the engine converges the instance may still be there, deleting.
		if view, err := h.Engine.Observer.ObserveInstance(ctx, in.Ref); err == nil {
			assert.True(t, view.Status.Deleting, "an instance still observed after an accepted deletion is deleting")
		}

		h.Converge(t, in.Ref)

		_, err := h.Engine.Observer.ObserveInstance(ctx, in.Ref)
		assert.True(t, errors.Is(err, engine.ErrNotFound), "observe: %v", err)
		_, _, err = h.Engine.DesiredState.GetInstance(ctx, in.Ref)
		assert.True(t, errors.Is(err, engine.ErrNotFound), "get: %v", err)
	})

	t.Run("writes and reads on a missing instance report ErrNotFound", func(t *testing.T) {
		h := newHarness(t)
		ghost := sample("demo", "ghost")

		_, err := h.Engine.DesiredState.UpdateInstance(ctx, ghost, engine.WriteOptions{})
		assert.True(t, errors.Is(err, engine.ErrNotFound), "update: %v", err)
		_, err = h.Engine.DesiredState.DeleteInstance(ctx, ghost.Ref, engine.WriteOptions{})
		assert.True(t, errors.Is(err, engine.ErrNotFound), "delete: %v", err)
		_, _, err = h.Engine.DesiredState.GetInstance(ctx, ghost.Ref)
		assert.True(t, errors.Is(err, engine.ErrNotFound), "get: %v", err)
		_, err = h.Engine.Observer.ObserveInstance(ctx, ghost.Ref)
		assert.True(t, errors.Is(err, engine.ErrNotFound), "observe: %v", err)
	})

	t.Run("projects do not see each other's instances", func(t *testing.T) {
		h := newHarness(t)
		accepted(t)(h.Engine.DesiredState.CreateInstance(ctx, sample("alpha", "jh"), engine.WriteOptions{}))
		accepted(t)(h.Engine.DesiredState.CreateInstance(ctx, sample("beta", "trino"), engine.WriteOptions{}))

		views, err := h.Engine.Observer.ObserveInstances(ctx, "alpha")
		require.NoError(t, err)
		require.Len(t, views, 1)
		assert.Equal(t, "jh", views[0].Ref.Name)

		desired, err := h.Engine.DesiredState.ListInstances(ctx, "beta")
		require.NoError(t, err)
		require.Len(t, desired, 1)
		assert.Equal(t, "trino", desired[0].Ref.Name)

		_, err = h.Engine.Observer.ObserveInstance(ctx, engine.InstanceRef{Project: "beta", Name: "jh"})
		assert.True(t, errors.Is(err, engine.ErrNotFound))
	})

	t.Run("a watch reports the creation and the convergence, and closes with its context", func(t *testing.T) {
		h := newHarness(t)
		watchCtx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		events, err := h.Engine.Observer.WatchInstances(watchCtx, "demo")
		require.NoError(t, err)

		in := sample("demo", "jh")
		accepted(t)(h.Engine.DesiredState.CreateInstance(ctx, in, engine.WriteOptions{}))
		added := next(t, events)
		assert.Equal(t, engine.EventAdded, added.Type)
		assert.Equal(t, in.Ref, added.Instance.Ref)

		h.Converge(t, in.Ref)
		var last engine.InstanceEvent
		for last.Instance.Status.Phase != engine.PhaseReady {
			last = next(t, events)
			assert.Equal(t, engine.EventModified, last.Type)
		}

		cancel()
		select {
		case _, ok := <-drain(events):
			assert.False(t, ok)
		case <-time.After(eventTimeout):
			t.Fatal("the watch did not close after its context ended")
		}
	})

	t.Run("a watch closes with its context even with an event left unread", func(t *testing.T) {
		h := newHarness(t)
		watchCtx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		events, err := h.Engine.Observer.WatchInstances(watchCtx, "demo")
		require.NoError(t, err)
		accepted(t)(h.Engine.DesiredState.CreateInstance(ctx, sample("demo", "jh"), engine.WriteOptions{}))

		cancel()
		select {
		case <-drain(events):
		case <-time.After(eventTimeout):
			t.Fatal("the watch did not close after its context ended")
		}
	})

	if !newHarness(t).Idempotency {
		return
	}

	t.Run("a replayed create answers the first operation", func(t *testing.T) {
		h := newHarness(t)
		in := sample("demo", "jh")
		opts := engine.WriteOptions{IdempotencyKey: "deploy-1"}
		first := accepted(t)(h.Engine.DesiredState.CreateInstance(ctx, in, opts))

		replay := accepted(t)(h.Engine.DesiredState.CreateInstance(ctx, in, opts))
		assert.Equal(t, first.DesiredRevision, replay.DesiredRevision)

		different := in
		different.Version = "2.0.0"
		op, err := h.Engine.DesiredState.CreateInstance(ctx, different, opts)
		rejectedWith(t, op, err, engine.CodeConflict)
	})
}

// drain returns a channel that yields once events is closed.
func drain(events <-chan engine.InstanceEvent) <-chan engine.InstanceEvent {
	done := make(chan engine.InstanceEvent)
	go func() {
		for range events {
		}
		close(done)
	}()
	return done
}
