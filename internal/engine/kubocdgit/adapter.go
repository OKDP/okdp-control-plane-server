// Package kubocdgit is the KuboCD engine with its desired state in git. The
// server commits one flat manifest per object under the store path (see
// gitstore for the layout), a sync tool (gitops.SyncAdapter) applies the
// branch, and the server observes the Releases the KuboCD controller
// reconciles. The platform Context stays in Kubernetes and is only read, the
// control plane Context with the service catalog lives in git.
package kubocdgit

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/client-go/dynamic"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocd"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

// Name is the engine type of this adapter, the same engine as kubocd.
const Name = kubocd.Name

const (
	defaultCacheTTL     = 30 * time.Second
	defaultPollInterval = 30 * time.Second
	defaultSyncTimeout  = 3 * time.Second
	defaultReadTimeout  = 5 * time.Second
	// syncWait is how long a write waits for its RequestSync before it
	// answers, the request going on in the background.
	syncWait            = 50 * time.Millisecond
	syncWatchBackoffMin = time.Second
	syncWatchBackoffMax = 30 * time.Second
	maxCASAttempts      = 5
)

// Deps are what the adapter reads and writes through.
type Deps struct {
	Store *gitstore.Store
	// Sync wakes the sync tool after a commit and reports what it applied.
	// Nil when no sync tool is observed: nothing is ever reported applied.
	Sync gitops.SyncAdapter
	// Platform reads the platform Context in Kubernetes. Never written.
	Platform repository.ContextRepository
	// Cluster reads the Releases, HelmReleases and Connections.
	Cluster  dynamic.Interface
	Packages kubocd.PackageSource
	Config   kubocd.Config
	// ControlPlaneContext names the control plane Context, stored in git as
	// contexts/<name>.yaml and applied in its namespace.
	ControlPlaneContext repository.ContextRef
	// Catalog reads the Kubernetes serviceCatalog a missing control plane
	// Context starts from. Nil: an empty catalog for the bootstrap, the
	// catalog Platform reads for a write.
	Catalog CatalogSource
	// InsecureRegistries lists registry hosts reached over plain HTTP.
	InsecureRegistries []string
	// CacheTTL bounds the age of the snapshot a read uses when nothing
	// reported a new revision (30s).
	CacheTTL time.Duration
	// PollInterval is how often a watch recomputes the instances when no
	// event woke it up (30s).
	PollInterval time.Duration
	// SyncTimeout bounds the RequestSync that follows a commit (3s). It runs
	// even when the caller's request was cancelled, the commit being durable,
	// and the write answers after 50ms at most while it goes on.
	SyncTimeout time.Duration
	// ReadFetchTimeout bounds the fetch a read makes to refresh an expired
	// snapshot (5s). Past it, the read serves the expired snapshot.
	ReadFetchTimeout time.Duration
}

// Adapter implements the four engine ports with the desired state in git.
type Adapter struct {
	store         *gitstore.Store
	sync          gitops.SyncAdapter
	platform      repository.ContextRepository
	cluster       dynamic.Interface
	packages      kubocd.PackageSource
	config        kubocd.Config
	controlPlane  repository.ContextRef
	catalogSource CatalogSource
	insecure      []string
	pollInterval  time.Duration
	syncTimeout   time.Duration
	snapshots     *snapshots

	mu          sync.Mutex
	wg          sync.WaitGroup
	syncState   gitops.SyncState
	syncWatched bool
	names       map[engine.InstanceRef]string
	subscribers map[chan struct{}]struct{}
}

// New returns the adapter. Start makes it follow the sync tool.
func New(deps Deps) *Adapter {
	ttl := deps.CacheTTL
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	poll := deps.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	syncTimeout := deps.SyncTimeout
	if syncTimeout <= 0 {
		syncTimeout = defaultSyncTimeout
	}
	readTimeout := deps.ReadFetchTimeout
	if readTimeout <= 0 {
		readTimeout = defaultReadTimeout
	}
	return &Adapter{
		store:         deps.Store,
		sync:          deps.Sync,
		platform:      deps.Platform,
		cluster:       deps.Cluster,
		packages:      deps.Packages,
		config:        deps.Config,
		controlPlane:  deps.ControlPlaneContext,
		catalogSource: deps.Catalog,
		insecure:      deps.InsecureRegistries,
		pollInterval:  poll,
		syncTimeout:   syncTimeout,
		snapshots:     newSnapshots(deps.Store, ttl, readTimeout),
		syncState:     gitops.SyncState{Phase: gitops.SyncUnknown},
		names:         map[engine.InstanceRef]string{},
		subscribers:   map[chan struct{}]struct{}{},
	}
}

// Engine bundles the four ports.
func (a *Adapter) Engine() engine.Engine {
	return engine.Engine{
		Info: engine.Info{
			Name:        Name,
			Persistence: engine.Persistence{Mode: engine.PersistenceGit},
		},
		DesiredState:   a,
		Observer:       a,
		Catalog:        a,
		PlatformConfig: a,
	}
}

// Start follows the sync tool until ctx ends: each new state is kept for the
// observation, and a new fetched or applied revision refreshes the snapshot
// and wakes the watches up.
func (a *Adapter) Start(ctx context.Context) {
	if a.sync == nil {
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			a.mu.Lock()
			a.syncWatched = false
			a.mu.Unlock()
		}()
		backoff := syncWatchBackoffMin
		for ctx.Err() == nil {
			states, err := a.sync.WatchSync(ctx)
			if err != nil {
				a.mu.Lock()
				a.syncWatched = false
				a.mu.Unlock()
				logrus.WithError(err).Warn("Cannot watch the sync state, retrying")
			} else {
				received := false
				for st := range states {
					received = true
					backoff = syncWatchBackoffMin
					a.setSyncState(st)
				}
				a.mu.Lock()
				a.syncWatched = false
				a.mu.Unlock()
				if received {
					backoff = syncWatchBackoffMin
				}
			}
			delay := syncWatchJitter(backoff)
			backoff = min(backoff*2, syncWatchBackoffMax)
			select {
			case <-ctx.Done():
			case <-time.After(delay):
			}
		}
	}()
}

// Wait blocks until background loops started by Start and in-flight
// RequestSync calls finish.
func (a *Adapter) Wait() {
	a.wg.Wait()
}

func syncWatchJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	quarter := d / 4
	if quarter <= 0 {
		return d
	}
	return d - quarter + rand.N(2*quarter)
}

func (a *Adapter) setSyncState(st gitops.SyncState) {
	a.mu.Lock()
	prev := a.syncState
	a.syncState = st
	a.syncWatched = true
	a.mu.Unlock()
	if st.FetchedRevision != prev.FetchedRevision || st.AppliedRevision != prev.AppliedRevision {
		a.snapshots.invalidate()
	}
	if syncStateChanged(prev, st) {
		a.notify()
	}
}

func syncStateChanged(a, b gitops.SyncState) bool {
	return a.Phase != b.Phase ||
		a.Message != b.Message ||
		a.FetchedRevision != b.FetchedRevision ||
		a.AppliedRevision != b.AppliedRevision ||
		a.AttemptedRevision != b.AttemptedRevision
}

// currentSync returns the state the watch keeps, or reads it when Start is
// not running.
func (a *Adapter) currentSync(ctx context.Context) gitops.SyncState {
	a.mu.Lock()
	st, watched := a.syncState, a.syncWatched
	a.mu.Unlock()
	if watched || a.sync == nil {
		return st
	}
	observed, err := a.sync.ObserveSync(ctx)
	if err != nil {
		return gitops.SyncState{Phase: gitops.SyncUnknown, Message: err.Error()}
	}
	return observed
}

// subscribe returns a channel that receives a wake-up after each local
// write and each sync state change.
func (a *Adapter) subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	a.mu.Lock()
	a.subscribers[ch] = struct{}{}
	a.mu.Unlock()
	return ch, func() {
		a.mu.Lock()
		delete(a.subscribers, ch)
		a.mu.Unlock()
	}
}

func (a *Adapter) notify() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for ch := range a.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// commit writes req as the caller of ctx, keeps the cache on the new commit
// and wakes the sync tool up.
func (a *Adapter) commit(ctx context.Context, req gitstore.WriteRequest) (*gitstore.WriteResult, error) {
	req.Author = gitstore.AuthorFromIdentity(auth.FromContext(ctx), gitstore.Author{})
	res, err := a.store.Write(ctx, req)
	if err != nil {
		if errors.Is(err, gitstore.ErrUncertain) {
			a.snapshots.invalidate()
			a.notify()
		}
		return nil, err
	}
	if res.Replayed {
		// The first commit of a replay may predate later changes.
		a.snapshots.invalidate()
	} else {
		a.snapshots.advance(ctx, res.Revision)
	}
	if !res.Unchanged && a.sync != nil {
		a.requestSync(ctx, res.Revision)
	}
	a.notify()
	return res, nil
}

// requestSync wakes the sync tool up for revision. It waits syncWait at most
// (less if ctx ends), the request going on up to syncTimeout in the
// background: a fast sync tool API is done before the write answers, a slow
// one does not hold it.
func (a *Adapter) requestSync(ctx context.Context, revision string) {
	done := make(chan struct{})
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer close(done)
		syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.syncTimeout)
		defer cancel()
		if err := a.sync.RequestSync(syncCtx, revision); err != nil {
			logrus.WithError(err).WithField("revision", revision).Warn("Could not wake the sync tool up, it applies the commit at its next interval")
		}
	}()
	wait := time.NewTimer(syncWait)
	defer wait.Stop()
	select {
	case <-done:
	case <-wait.C:
	case <-ctx.Done():
	}
}

// notFoundError keeps the store error and also matches engine.ErrNotFound.
type notFoundError struct{ err error }

func (e *notFoundError) Error() string   { return e.err.Error() }
func (e *notFoundError) Unwrap() []error { return []error{engine.ErrNotFound, e.err} }

func notFound(format string, args ...any) error {
	return &notFoundError{err: fmt.Errorf(format, args...)}
}

// failure classifies a failed write the way the engine ports report it.
func failure(err error, exists string) (engine.Operation, error) {
	switch {
	case errors.Is(err, gitstore.ErrIdempotencyConflict), errors.Is(err, gitstore.ErrConflict):
		return engine.Rejected(engine.CodeConflict, err.Error()), nil
	case errors.Is(err, gitstore.ErrAlreadyExists):
		return engine.Rejected(exists, err.Error()), nil
	case errors.Is(err, gitstore.ErrRejected), errors.Is(err, gitstore.ErrInvalidRequest), errors.Is(err, gitstore.ErrInvalidPath):
		return engine.Rejected(engine.CodeRejected, err.Error()), nil
	case errors.Is(err, gitstore.ErrUncertain):
		return engine.Operation{Outcome: engine.OutcomeUnknown, Message: err.Error()}, nil
	case errors.Is(err, gitstore.ErrNotFound):
		return engine.Operation{}, &notFoundError{err: err}
	}
	return engine.Operation{}, err
}

// validRevision reports whether rev can be a file revision. Anything else
// (a KuboCD generation kept by the console across a mode switch) is stale.
func validRevision(rev string) bool {
	if len(rev) != 40 {
		return false
	}
	for _, r := range rev {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
