package kubocdgit

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
)

type inflightFetch struct {
	done    chan struct{}
	bounded bool
	snap    *gitstore.Snapshot
	err     error
}

// snapshots caches the last snapshot of the branch, so that a read does not
// fetch the remote. A local write moves it to the commit it made, a new
// revision reported by the sync tool or the TTL makes the next read fetch.
// When a fetch fails, the last snapshot keeps being served.
//
// Cached reads take mu.RLock and never block on an in-flight network fetch.
// Concurrent misses coalesce onto a shared in-flight fetch call.
type snapshots struct {
	store *gitstore.Store
	ttl   time.Duration
	// readTimeout bounds the fetch a read makes to refresh an expired
	// snapshot, so that a silent remote delays it that long at most.
	readTimeout time.Duration
	// fetch is store.Latest, a seam for the tests.
	fetch func(ctx context.Context) (*gitstore.Snapshot, error)

	mu   sync.RWMutex
	snap *gitstore.Snapshot
	at   time.Time

	fetchMu  sync.Mutex
	inflight *inflightFetch

	memoMu    sync.Mutex
	fileRevs  map[string]string
	ancestors map[[2]string]bool
}

const maxAncestorMemo = 10000

func newSnapshots(store *gitstore.Store, ttl, readTimeout time.Duration) *snapshots {
	return &snapshots{
		store: store, ttl: ttl, readTimeout: readTimeout, fetch: store.Latest,
		fileRevs: map[string]string{}, ancestors: map[[2]string]bool{},
	}
}

// current returns the cached snapshot, fetching when it is older than the
// TTL. Refreshing an expired snapshot waits readTimeout at most, after which
// the expired one is served. The first fetch, with nothing to serve, waits
// for the store's own timeout.
func (c *snapshots) current(ctx context.Context) (*gitstore.Snapshot, error) {
	c.mu.RLock()
	snap, at := c.snap, c.at
	c.mu.RUnlock()
	if snap != nil && time.Since(at) < c.ttl {
		return snap, nil
	}
	if snap == nil {
		return c.fetchWithFallback(ctx, false)
	}
	return c.fetchWithFallback(ctx, true)
}

// latest fetches the branch whatever the age of the cached snapshot. A write
// reads what it modifies through it.
func (c *snapshots) latest(ctx context.Context) (*gitstore.Snapshot, error) {
	return c.fetchWithFallback(ctx, false)
}

// refresh fetches the branch whatever the age of the cached snapshot, like
// latest, for a read that must see the commits the sync tool reports. With a
// snapshot to fall back on, it waits readTimeout at most and serves that one
// past it.
func (c *snapshots) refresh(ctx context.Context) (*gitstore.Snapshot, error) {
	c.mu.RLock()
	hasSnap := c.snap != nil
	c.mu.RUnlock()
	return c.fetchWithFallback(ctx, hasSnap)
}

// strict fetches the branch and fails when the fetch fails, for a decision
// that must not rest on a stale snapshot.
func (c *snapshots) strict(ctx context.Context) (*gitstore.Snapshot, error) {
	snap, err := c.fetch(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.RLock()
	prev := c.snap
	c.mu.RUnlock()

	canAdvance := prev == nil || prev.Revision() == "" || snap.Revision() == ""
	if !canAdvance {
		if ok, err := c.isAncestor(ctx, prev.Revision(), snap.Revision()); err == nil && ok {
			canAdvance = true
		}
	}
	if canAdvance {
		c.mu.Lock()
		if c.snap == prev {
			c.setLocked(snap)
		}
		c.mu.Unlock()
	}
	return snap, nil
}

// fetchWithFallback performs a coalesced network fetch without holding mu so
// cached reads never stall. When bounded is true, the fetch is capped by
// readTimeout. On failure, if a cached snapshot exists it is served and c.at
// is refreshed only when the caller's ctx is still active (neither canceled
// nor expired).
func (c *snapshots) fetchWithFallback(callerCtx context.Context, bounded bool) (*gitstore.Snapshot, error) {
	fetchCtx := callerCtx
	cancel := func() {}
	if bounded {
		fetchCtx, cancel = context.WithTimeout(callerCtx, c.readTimeout)
	}
	defer cancel()

	var (
		call   *inflightFetch
		leader bool
	)
	c.fetchMu.Lock()
	if c.inflight != nil && (bounded || !c.inflight.bounded) {
		call = c.inflight
	} else {
		call = &inflightFetch{done: make(chan struct{}), bounded: bounded}
		if c.inflight == nil {
			c.inflight = call
		}
		leader = true
	}
	c.fetchMu.Unlock()

	if leader {
		snap, err := c.fetch(fetchCtx)
		call.snap, call.err = snap, err
		if err == nil {
			c.mu.Lock()
			c.setLocked(snap)
			c.mu.Unlock()
		}
		c.fetchMu.Lock()
		if c.inflight == call {
			c.inflight = nil
		}
		c.fetchMu.Unlock()
		close(call.done)
	} else {
		select {
		case <-call.done:
		case <-fetchCtx.Done():
			return c.fallback(callerCtx, fetchCtx.Err())
		}
	}

	if call.err != nil {
		return c.fallback(callerCtx, call.err)
	}
	return call.snap, nil
}

func (c *snapshots) fallback(callerCtx context.Context, err error) (*gitstore.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap != nil {
		// The next fetch waits for the TTL: retrying on every read would
		// make each one wait for the unreachable remote. A cancelled or
		// timed-out caller context does not mean the remote failed, so leave
		// c.at alone for the next request.
		if callerCtx.Err() == nil {
			c.at = time.Now()
		}
		logrus.WithError(err).Warn("Serving the last known git snapshot, the fetch failed")
		return c.snap, nil
	}
	return nil, err
}

// advance moves the cache to revision, a commit this server just made. It
// never moves the cache back to an older commit.
func (c *snapshots) advance(ctx context.Context, revision string) {
	snap, err := c.store.At(ctx, revision)
	if err != nil {
		c.invalidate()
		return
	}
	c.mu.RLock()
	prev := c.snap
	c.mu.RUnlock()
	if prev != nil && prev.Revision() != "" {
		if ok, err := c.isAncestor(ctx, prev.Revision(), revision); err != nil || !ok {
			c.invalidate()
			return
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap == prev {
		c.setLocked(snap)
	} else {
		c.at = time.Time{}
	}
}

// invalidate makes the next read fetch. The cached snapshot stays as the
// fallback of a failed fetch.
func (c *snapshots) invalidate() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}

func (c *snapshots) setLocked(snap *gitstore.Snapshot) {
	if c.snap == nil || c.snap.Revision() != snap.Revision() {
		c.memoMu.Lock()
		c.fileRevs = map[string]string{}
		c.memoMu.Unlock()
	}
	c.snap = snap
	c.at = time.Now()
}

// fileRevision is snap.FileRevision, memoised for the cached snapshot.
func (c *snapshots) fileRevision(snap *gitstore.Snapshot, path string) (string, error) {
	key := snap.Revision() + "\x00" + path
	c.memoMu.Lock()
	rev, ok := c.fileRevs[key]
	c.memoMu.Unlock()
	if ok {
		return rev, nil
	}
	rev, err := snap.FileRevision(path)
	if err != nil {
		return "", err
	}
	c.memoMu.Lock()
	c.fileRevs[key] = rev
	c.memoMu.Unlock()
	return rev, nil
}

// isAncestor is store.IsAncestor, memoised: the answer for two commits never changes.
func (c *snapshots) isAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	key := [2]string{ancestor, descendant}
	c.memoMu.Lock()
	ok, found := c.ancestors[key]
	c.memoMu.Unlock()
	if found {
		return ok, nil
	}
	ok, err := c.store.IsAncestor(ctx, ancestor, descendant)
	if err != nil {
		return false, err
	}
	c.memoMu.Lock()
	if len(c.ancestors) >= maxAncestorMemo {
		c.ancestors = map[[2]string]bool{}
	}
	c.ancestors[key] = ok
	c.memoMu.Unlock()
	return ok, nil
}
