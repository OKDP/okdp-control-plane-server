package flux

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
)

func receive(t *testing.T, ch <-chan gitops.SyncState) gitops.SyncState {
	t.Helper()
	select {
	case st, ok := <-ch:
		require.True(t, ok, "channel closed")
		return st
	case <-time.After(5 * time.Second):
		t.Fatal("no state received")
	}
	return gitops.SyncState{}
}

func waitClosed(t *testing.T, ch <-chan gitops.SyncState) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed")
		}
	}
}

func update(t *testing.T, a *Adapter, obj *unstructured.Unstructured) {
	t.Helper()
	ri := a.repos()
	if obj.GetKind() == kustomizationKind {
		ri = a.kustomizations()
	}
	_, err := ri.Update(context.Background(), obj, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func TestWatchSyncFollowsACommitFromFetchToApply(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	a := newAdapter(t, client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.WatchSync(ctx)
	require.NoError(t, err)
	st := receive(t, ch)
	assert.Equal(t, gitops.SyncReady, st.Phase)
	assert.Equal(t, shaA, st.AppliedRevision)

	update(t, a, gitRepository(shaB))
	st = receive(t, ch)
	assert.Equal(t, gitops.SyncProgressing, st.Phase)
	assert.Equal(t, shaB, st.FetchedRevision)
	assert.Equal(t, shaA, st.AppliedRevision)

	update(t, a, kustomization(shaB))
	st = receive(t, ch)
	assert.Equal(t, gitops.SyncReady, st.Phase)
	assert.Equal(t, shaB, st.AppliedRevision)
}

func TestWatchSyncReportsAKustomizationFailure(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	a := newAdapter(t, client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.WatchSync(ctx)
	require.NoError(t, err)
	receive(t, ch)

	update(t, a, gitRepository(shaB))
	receive(t, ch)
	update(t, a, failingKustomization(t, shaA, shaB))
	st := receive(t, ch)
	assert.Equal(t, gitops.SyncFailed, st.Phase)
	assert.Equal(t, shaA, st.AppliedRevision)
	assert.Equal(t, shaB, st.AttemptedRevision)
	assert.Contains(t, st.Message, "dry-run failed")
}

func TestWatchSyncSkipsUnchangedStatesAndOtherObjects(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	a := newAdapter(t, client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.WatchSync(ctx)
	require.NoError(t, err)
	receive(t, ch)

	// A label change gives the same state, and another GitRepository in the
	// namespace is ignored.
	labelled := gitRepository(shaA)
	labelled.SetLabels(map[string]string{"team": "platform"})
	update(t, a, labelled)
	other := gitRepository(shaB)
	other.SetName("other")
	set(t, other, true, "spec", "suspend")
	_, err = a.repos().Create(ctx, other, metav1.CreateOptions{})
	require.NoError(t, err)

	update(t, a, set(t, kustomization(shaA), true, "spec", "suspend"))
	st := receive(t, ch)
	assert.Equal(t, gitops.SyncSuspended, st.Phase, "the next state sent is the suspension")
	assert.Contains(t, st.Message, "Kustomization okdp/okdp-state")
}

func TestWatchSyncUsesANameFieldSelector(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := newAdapter(t, client).WatchSync(ctx)
	require.NoError(t, err)

	watched := map[string]string{}
	for _, act := range client.Actions() {
		if w, ok := act.(k8stesting.WatchAction); ok {
			watched[w.GetResource().Resource] = w.GetWatchRestrictions().Fields.String()
		}
	}
	assert.Equal(t, map[string]string{
		"gitrepositories": "metadata.name=" + repoName,
		"kustomizations":  "metadata.name=" + ksName,
	}, watched)
}

func TestWatchSyncReportsADeletedObjectAsUnknown(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	a := newAdapter(t, client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.WatchSync(ctx)
	require.NoError(t, err)
	receive(t, ch)

	require.NoError(t, a.kustomizations().Delete(ctx, ksName, metav1.DeleteOptions{}))
	st := receive(t, ch)
	assert.Equal(t, gitops.SyncUnknown, st.Phase)
	assert.Contains(t, st.Message, "not found")
}

func TestWatchSyncStartsUnknownWhenAnObjectIsMissing(t *testing.T) {
	a := newAdapter(t, newClient(gitRepository(shaA)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.WatchSync(ctx)
	require.NoError(t, err)
	assert.Equal(t, gitops.SyncUnknown, receive(t, ch).Phase)

	_, err = a.kustomizations().Create(ctx, kustomization(shaA), metav1.CreateOptions{})
	require.NoError(t, err)
	assert.Equal(t, gitops.SyncReady, receive(t, ch).Phase)
}

func TestWatchSyncClosesWhenTheContextEnds(t *testing.T) {
	a := newAdapter(t, newClient(gitRepository(shaA), kustomization(shaA)))
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := a.WatchSync(ctx)
	require.NoError(t, err)
	receive(t, ch)
	cancel()
	waitClosed(t, ch)
}

func TestWatchSyncClosesWhenAWatchEnds(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	ksWatch := watch.NewFake()
	client.PrependWatchReactor("kustomizations", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, ksWatch, nil
	})
	ch, err := newAdapter(t, client).WatchSync(context.Background())
	require.NoError(t, err)
	receive(t, ch)

	ksWatch.Error(&metav1.Status{Reason: metav1.StatusReasonExpired, Message: "too old resource version"})
	waitClosed(t, ch)
}

func TestWatchSyncDeliversTheInitialStateThenTheLatestToASlowReader(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	ksWatch := watch.NewFake()
	client.PrependWatchReactor("kustomizations", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, ksWatch, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := newAdapter(t, client).WatchSync(ctx)
	require.NoError(t, err)

	// Nothing is read yet. NewFake is unbuffered, so each Modify returns once
	// the adapter took the event, and the last one, which changes nothing,
	// makes sure the one before was folded.
	ksWatch.Modify(set(t, kustomization(shaA), true, "spec", "suspend"))
	ksWatch.Modify(failingKustomization(t, shaA, shaB))
	ksWatch.Modify(kustomization(shaB))
	labelled := kustomization(shaB)
	labelled.SetLabels(map[string]string{"x": "y"})
	ksWatch.Modify(labelled)

	first := receive(t, ch)
	assert.Equal(t, gitops.SyncReady, first.Phase, "the initial state comes first")
	assert.Equal(t, shaA, first.AppliedRevision)

	// After the initial state, the reader again falls behind two changes.
	ksWatch.Modify(set(t, kustomization(shaB), true, "spec", "suspend"))
	ksWatch.Modify(failingKustomization(t, shaA, shaB))
	ksWatch.Modify(kustomization(shaB))
	ksWatch.Modify(labelled)
	latest := receive(t, ch)
	assert.Equal(t, gitops.SyncProgressing, latest.Phase)
	assert.Equal(t, shaB, latest.AppliedRevision)
	select {
	case extra := <-ch:
		t.Fatalf("unexpected extra state %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWatchSyncReturnsAListError(t *testing.T) {
	client := newClient()
	client.PrependReactor("list", "gitrepositories", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, assert.AnError
	})
	_, err := newAdapter(t, client).WatchSync(context.Background())
	assert.ErrorIs(t, err, assert.AnError)
}
