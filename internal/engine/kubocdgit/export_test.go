package kubocdgit

import "github.com/okdp/okdp-control-plane-server/internal/gitops"

// SyncState is the sync state the adapter keeps, for the tests to wait on.
func (a *Adapter) SyncState() gitops.SyncState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.syncState
}
