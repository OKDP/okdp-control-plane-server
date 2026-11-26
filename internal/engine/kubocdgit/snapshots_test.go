package kubocdgit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
)

func TestAFailedFetchServesTheLastSnapshotForAWholeTTL(t *testing.T) {
	bin, err := exec.LookPath("git")
	require.NoError(t, err, "this test needs the git binary")
	bare := filepath.Join(t.TempDir(), "remote.git")
	out, err := exec.Command(bin, "init", "--quiet", "--bare", "--initial-branch=main", bare).CombinedOutput()
	require.NoError(t, err, "%s", out)
	store, err := gitstore.New(gitstore.Config{Repository: gitops.Repository{URL: bare, Branch: "main", Path: "clusters/dev"}})
	require.NoError(t, err)
	ctx := context.Background()
	_, err = store.Bootstrap(ctx)
	require.NoError(t, err)

	c := newSnapshots(store, time.Hour, defaultReadTimeout)
	var fetches atomic.Int32
	c.fetch = func(ctx context.Context) (*gitstore.Snapshot, error) {
		fetches.Add(1)
		return store.Latest(ctx)
	}
	first, err := c.current(ctx)
	require.NoError(t, err)

	// The forge becomes unreachable and the cache expires.
	require.NoError(t, os.Rename(bare, bare+".away"))
	t.Cleanup(func() { _ = os.Rename(bare+".away", bare) })
	c.invalidate()

	served, err := c.current(ctx)
	require.NoError(t, err)
	assert.Equal(t, first.Revision(), served.Revision())
	require.Equal(t, int32(2), fetches.Load())

	for i := 0; i < 5; i++ {
		served, err := c.current(ctx)
		require.NoError(t, err)
		assert.Equal(t, first.Revision(), served.Revision())
	}
	assert.Equal(t, int32(2), fetches.Load(), "no fetch until the TTL runs out again")
}

func TestASilentForgeDelaysAReadByTheReadTimeoutOnly(t *testing.T) {
	c := newSnapshots(nil, time.Hour, 30*time.Millisecond)
	ctx := context.Background()
	var fetches atomic.Int32
	known := &gitstore.Snapshot{}
	var bounded []bool
	c.fetch = func(ctx context.Context) (*gitstore.Snapshot, error) {
		fetches.Add(1)
		_, ok := ctx.Deadline()
		bounded = append(bounded, ok)
		return known, nil
	}
	first, err := c.current(ctx)
	require.NoError(t, err)
	_, err = c.latest(ctx)
	require.NoError(t, err)
	_, err = c.strict(ctx)
	require.NoError(t, err)
	assert.Equal(t, []bool{false, false, false}, bounded, "the first fetch, latest and strict keep the store's timeout")
	fetches.Store(1)

	// The forge accepts the connection and never answers.
	var deadline atomic.Bool
	c.fetch = func(ctx context.Context) (*gitstore.Snapshot, error) {
		fetches.Add(1)
		_, bounded := ctx.Deadline()
		deadline.Store(bounded)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	c.invalidate()

	started := time.Now()
	served, err := c.current(ctx)
	elapsed := time.Since(started)
	require.NoError(t, err)
	assert.Same(t, first, served, "the expired snapshot is served")
	assert.GreaterOrEqual(t, elapsed, 30*time.Millisecond, "the refresh waited for the read timeout")
	assert.Less(t, elapsed, time.Second, "bounded by the read timeout, not by the store's 2 minutes")
	assert.Equal(t, int32(2), fetches.Load())
	assert.True(t, deadline.Load(), "the refresh carries the read timeout")

	_, err = c.current(ctx)
	require.NoError(t, err)
	assert.Equal(t, int32(2), fetches.Load(), "the failed refresh restarts the TTL")
}

func TestARefreshIsBoundedOnlyWhenASnapshotIsCached(t *testing.T) {
	c := newSnapshots(nil, time.Hour, 30*time.Millisecond)
	ctx := context.Background()
	known := &gitstore.Snapshot{}
	var bounded []bool
	c.fetch = func(ctx context.Context) (*gitstore.Snapshot, error) {
		_, ok := ctx.Deadline()
		bounded = append(bounded, ok)
		return known, nil
	}
	_, err := c.refresh(ctx)
	require.NoError(t, err)
	_, err = c.refresh(ctx)
	require.NoError(t, err)
	assert.Equal(t, []bool{false, true}, bounded, "nothing to serve: the store's timeout, a cached snapshot: the read timeout")

	c.fetch = func(ctx context.Context) (*gitstore.Snapshot, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	started := time.Now()
	served, err := c.refresh(ctx)
	require.NoError(t, err)
	assert.Same(t, known, served, "a silent forge serves the cached snapshot")
	assert.Less(t, time.Since(started), time.Second)
}

func TestCachedReadsDoNotBlockDuringSlowRefreshAndConcurrentMissesCoalesce(t *testing.T) {
	c := newSnapshots(nil, time.Hour, 5*time.Second)
	ctx := context.Background()
	first := &gitstore.Snapshot{}
	second := &gitstore.Snapshot{}

	c.fetch = func(context.Context) (*gitstore.Snapshot, error) { return first, nil }
	got, err := c.current(ctx)
	require.NoError(t, err)
	require.Same(t, first, got)

	// Start a slow refresh and verify a fresh cached current() returns immediately.
	inFetch := make(chan struct{})
	release := make(chan struct{})
	var fetches atomic.Int32
	c.fetch = func(context.Context) (*gitstore.Snapshot, error) {
		fetches.Add(1)
		close(inFetch)
		<-release
		return second, nil
	}
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		_, _ = c.refresh(ctx)
	}()
	<-inFetch

	started := time.Now()
	cached, err := c.current(ctx)
	require.NoError(t, err)
	assert.Same(t, first, cached)
	assert.Less(t, time.Since(started), 100*time.Millisecond, "cached read must not block on an in-flight fetch")

	close(release)
	<-refreshDone

	// Concurrent misses coalesce into a single in-flight fetch.
	c.invalidate()
	fetches.Store(0)
	release2 := make(chan struct{})
	startedGate := make(chan struct{}, 1)
	c.fetch = func(context.Context) (*gitstore.Snapshot, error) {
		fetches.Add(1)
		select {
		case startedGate <- struct{}{}:
		default:
		}
		<-release2
		return second, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := c.current(ctx)
			assert.NoError(t, err)
			assert.Same(t, second, snap)
		}()
	}
	<-startedGate
	time.Sleep(20 * time.Millisecond)
	close(release2)
	wg.Wait()
	assert.Equal(t, int32(1), fetches.Load(), "concurrent cache misses should coalesce into one fetch")
}

func TestCallerCancelOrDeadlineDoesNotRefreshCacheTimestamp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mkCtx func() (context.Context, context.CancelFunc)
	}{
		{
			name: "canceled",
			mkCtx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
		},
		{
			name: "deadline exceeded",
			mkCtx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 10*time.Millisecond)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newSnapshots(nil, time.Hour, time.Second)
			first := &gitstore.Snapshot{}
			second := &gitstore.Snapshot{}
			c.fetch = func(context.Context) (*gitstore.Snapshot, error) { return first, nil }
			_, err := c.current(context.Background())
			require.NoError(t, err)

			c.invalidate()
			var fetches atomic.Int32
			c.fetch = func(ctx context.Context) (*gitstore.Snapshot, error) {
				fetches.Add(1)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			callerCtx, cancel := tc.mkCtx()
			defer cancel()

			served, err := c.current(callerCtx)
			require.NoError(t, err)
			assert.Same(t, first, served)
			assert.Equal(t, int32(1), fetches.Load())

			// Next request with a healthy context must still fetch immediately.
			c.fetch = func(context.Context) (*gitstore.Snapshot, error) {
				fetches.Add(1)
				return second, nil
			}
			refreshed, err := c.current(context.Background())
			require.NoError(t, err)
			assert.Same(t, second, refreshed)
			assert.Equal(t, int32(2), fetches.Load(), "caller ctx cancellation/deadline must not suppress the next fetch")
		})
	}
}
