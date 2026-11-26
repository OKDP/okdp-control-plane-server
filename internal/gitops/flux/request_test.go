package flux

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
)

type recordedPatch struct {
	value string
	// covered is the number of RequestSync calls counted when the patch
	// reached the API server.
	covered uint64
}

// patchRecorder records the GitRepository patches. When gate is set, the
// first patch waits for it to close and then returns firstErr.
type patchRecorder struct {
	mu       sync.Mutex
	patches  []recordedPatch
	started  chan struct{}
	gate     chan struct{}
	firstErr error
}

func recordPatches(t *testing.T, client *k8stesting.Fake, a *Adapter, rec *patchRecorder) {
	rec.started = make(chan struct{}, 100)
	client.PrependReactor("patch", "gitrepositories", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pa := action.(k8stesting.PatchAction)
		assert.Equal(t, types.MergePatchType, pa.GetPatchType())
		var body struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		require.NoError(t, json.Unmarshal(pa.GetPatch(), &body))

		a.mu.Lock()
		covered := a.requested
		a.mu.Unlock()
		rec.mu.Lock()
		first := len(rec.patches) == 0
		rec.patches = append(rec.patches, recordedPatch{body.Metadata.Annotations[ReconcileRequestAnnotation], covered})
		rec.mu.Unlock()
		rec.started <- struct{}{}

		if first && rec.gate != nil {
			<-rec.gate
			if rec.firstErr != nil {
				return true, nil, rec.firstErr
			}
		}
		return false, nil, nil
	})
}

func (r *patchRecorder) all() []recordedPatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedPatch(nil), r.patches...)
}

func requestedAt(t *testing.T, a *Adapter) string {
	t.Helper()
	obj, err := a.repos().Get(context.Background(), repoName, metav1.GetOptions{})
	require.NoError(t, err)
	return obj.GetAnnotations()[ReconcileRequestAnnotation]
}

func TestRequestSyncAnnotatesOnlyTheGitRepository(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	a := newAdapter(t, client)

	require.NoError(t, a.RequestSync(context.Background(), shaB))

	assert.Equal(t, "2026-11-22T10:00:00Z", requestedAt(t, a))
	actions := client.Actions()
	require.Len(t, actions, 2) // the patch, then the Get of requestedAt
	patch, ok := actions[0].(k8stesting.PatchAction)
	require.True(t, ok)
	assert.Equal(t, GitRepositoryResource, patch.GetResource())
	assert.Equal(t, repoName, patch.GetName())
	assert.Equal(t, types.MergePatchType, patch.GetPatchType())
	assert.JSONEq(t, `{"metadata":{"annotations":{"reconcile.fluxcd.io/requestedAt":"2026-11-22T10:00:00Z"}}}`, string(patch.GetPatch()))
	for _, act := range actions {
		assert.NotEqual(t, KustomizationResource, act.GetResource(), "the Kustomization is never touched")
	}
}

func TestRequestSyncSetsANewValueEachTimeUnderAFrozenClock(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	a := newAdapter(t, client)

	var values []string
	for range 3 {
		require.NoError(t, a.RequestSync(context.Background(), shaB))
		values = append(values, requestedAt(t, a))
	}
	assert.Equal(t, []string{
		"2026-11-22T10:00:00Z",
		"2026-11-22T10:00:00.000000001Z",
		"2026-11-22T10:00:00.000000002Z",
	}, values)
}

func TestRequestSyncFollowsAClockThatAdvances(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	cfg := testConfig()
	now := t0
	cfg.Now = func() time.Time { now = now.Add(time.Second); return now }
	a, err := New(client, cfg)
	require.NoError(t, err)

	require.NoError(t, a.RequestSync(context.Background(), shaA))
	require.NoError(t, a.RequestSync(context.Background(), shaB))
	assert.Equal(t, "2026-11-22T10:00:02Z", requestedAt(t, a))
}

// waitRequested waits until n RequestSync calls are counted.
func waitRequested(t *testing.T, a *Adapter, n uint64) {
	t.Helper()
	require.Eventually(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.requested == n
	}, 5*time.Second, time.Millisecond)
}

func TestRequestSyncCoalescesABurstIntoOnePatchAfterTheLastCall(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	a := newAdapter(t, client)
	rec := &patchRecorder{gate: make(chan struct{})}
	recordPatches(t, &client.Fake, a, rec)

	firstDone := make(chan error, 1)
	go func() { firstDone <- a.RequestSync(context.Background(), "c0") }()
	<-rec.started // the first patch is in flight

	const burst = 20
	var wg sync.WaitGroup
	errs := make(chan error, burst)
	for range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- a.RequestSync(context.Background(), "burst")
		}()
	}
	waitRequested(t, a, burst+1)
	close(rec.gate)

	require.NoError(t, <-firstDone)
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}

	patches := rec.all()
	require.Len(t, patches, 2, "the burst shares one patch")
	assert.Equal(t, uint64(1), patches[0].covered)
	assert.Equal(t, uint64(burst+1), patches[1].covered, "the second patch starts after the last call")
	assert.NotEqual(t, patches[0].value, patches[1].value)
	assert.Equal(t, patches[1].value, requestedAt(t, a))
}

func TestRequestSyncSharesTheErrorOfThePatchThatCoveredIt(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	a := newAdapter(t, client)
	unavailable := errors.New("apiserver unavailable")
	rec := &patchRecorder{gate: make(chan struct{}), firstErr: unavailable}
	recordPatches(t, &client.Fake, a, rec)

	firstDone := make(chan error, 1)
	go func() { firstDone <- a.RequestSync(context.Background(), "c0") }()
	<-rec.started

	secondDone := make(chan error, 1)
	go func() { secondDone <- a.RequestSync(context.Background(), "c1") }()
	waitRequested(t, a, 2)
	close(rec.gate)

	assert.ErrorIs(t, <-firstDone, unavailable)
	// The second call arrived after the first patch started: it gets its
	// own patch, which succeeds.
	assert.NoError(t, <-secondDone)
	assert.Len(t, rec.all(), 2)

	// A call covered by a failed patch gets its error.
	a.mu.Lock()
	a.requested, a.failedUpTo, a.failErr = 10, 11, unavailable
	a.mu.Unlock()
	assert.ErrorIs(t, a.RequestSync(context.Background(), "c2"), unavailable)
}

func TestRequestSyncDoesNotFailOthersWhenTheCallerCancels(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	a := newAdapter(t, client)
	rec := &patchRecorder{gate: make(chan struct{}), firstErr: context.Canceled}
	recordPatches(t, &client.Fake, a, rec)

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { firstDone <- a.RequestSync(ctx, "c0") }()
	<-rec.started

	secondDone := make(chan error, 1)
	go func() { secondDone <- a.RequestSync(context.Background(), "c1") }()
	waitRequested(t, a, 2)
	cancel()
	close(rec.gate)

	assert.ErrorIs(t, <-firstDone, context.Canceled)
	assert.NoError(t, <-secondDone, "the waiter patches again with its own context")
	patches := rec.all()
	require.Len(t, patches, 2)
	assert.Equal(t, patches[1].value, requestedAt(t, a))
}

func TestRequestSyncWaiterLeavesWhenItsContextEnds(t *testing.T) {
	client := newClient(gitRepository(shaA), kustomization(shaA))
	a := newAdapter(t, client)
	rec := &patchRecorder{gate: make(chan struct{})}
	recordPatches(t, &client.Fake, a, rec)

	firstDone := make(chan error, 1)
	go func() { firstDone <- a.RequestSync(context.Background(), "c0") }()
	<-rec.started

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.RequestSync(ctx, "c1") }()
	waitRequested(t, a, 2)
	cancel()
	assert.ErrorIs(t, <-done, context.Canceled)
	close(rec.gate)
	assert.NoError(t, <-firstDone)
}

func TestRequestSyncReportsAMissingGitRepository(t *testing.T) {
	a := newAdapter(t, newClient(kustomization(shaA)))
	err := a.RequestSync(context.Background(), shaA)
	assert.ErrorContains(t, err, "GitRepository okdp/okdp-state")
}
