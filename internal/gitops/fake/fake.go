// Package fake is an in-memory gitops.SyncAdapter for the tests of its
// consumers: it records the sync requests and reports the state a test sets.
package fake

import (
	"context"
	"sync"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
)

// Adapter is a gitops.SyncAdapter. Its zero value is not usable, see New.
type Adapter struct {
	mu       sync.Mutex
	state    gitops.SyncState
	requests []string
	watchers map[chan gitops.SyncState]struct{}

	// ValidateErr and RequestErr, when set, are returned by Validate and
	// RequestSync. A failed RequestSync records nothing.
	ValidateErr error
	RequestErr  error
}

var _ gitops.SyncAdapter = (*Adapter)(nil)

// New returns an adapter reporting an Unknown state.
func New() *Adapter {
	return &Adapter{
		state:    gitops.SyncState{Phase: gitops.SyncUnknown},
		watchers: map[chan gitops.SyncState]struct{}{},
	}
}

func (a *Adapter) Validate(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ValidateErr
}

func (a *Adapter) RequestSync(_ context.Context, revision string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.RequestErr != nil {
		return a.RequestErr
	}
	a.requests = append(a.requests, revision)
	return nil
}

// Requests returns the revisions passed to RequestSync, in order.
func (a *Adapter) Requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requests...)
}

func (a *Adapter) ObserveSync(context.Context) (gitops.SyncState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state, nil
}

// SetState changes the reported state and sends it to the watchers. A
// watcher that has not read the previous state gets only the latest one.
// A zero ObservedAt is set to now.
func (a *Adapter) SetState(st gitops.SyncState) {
	if st.ObservedAt.IsZero() {
		st.ObservedAt = time.Now()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = st
	for ch := range a.watchers {
		sendLatest(ch, st)
	}
}

func (a *Adapter) WatchSync(ctx context.Context) (<-chan gitops.SyncState, error) {
	ch := make(chan gitops.SyncState, 1)
	a.mu.Lock()
	ch <- a.state
	a.watchers[ch] = struct{}{}
	a.mu.Unlock()

	go func() {
		<-ctx.Done()
		a.mu.Lock()
		delete(a.watchers, ch)
		close(ch)
		a.mu.Unlock()
	}()
	return ch, nil
}

// sendLatest replaces an unread state with st. The caller holds a.mu, so it
// is the only sender.
func sendLatest(ch chan gitops.SyncState, st gitops.SyncState) {
	select {
	case ch <- st:
	default:
		select {
		case <-ch:
		default:
		}
		ch <- st
	}
}
