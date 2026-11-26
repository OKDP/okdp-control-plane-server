// Package gitops holds the contract between the control plane and whatever
// applies a git repository to the cluster (Flux, Argo CD). It imports neither:
// an adapter per tool implements SyncAdapter.
package gitops

import (
	"context"
	"time"
)

// Repository is the git location the desired state is committed to and the
// sync objects pull from. Path is relative to the repository root, without
// leading or trailing slash, empty for the root itself.
type Repository struct {
	URL    string
	Branch string
	Path   string
}

// SyncPhase is how far the sync tool got with the branch, in a vocabulary
// shared by all adapters.
type SyncPhase string

const (
	// SyncUnknown: the adapter could not read the state of the sync objects.
	SyncUnknown SyncPhase = "Unknown"
	// SyncProgressing: a revision is being fetched or applied.
	SyncProgressing SyncPhase = "Progressing"
	// SyncReady: AppliedRevision is applied and the last attempt succeeded.
	SyncReady SyncPhase = "Ready"
	// SyncFailed: the last fetch or apply failed, Message says why.
	// AppliedRevision is still the last revision applied successfully,
	// AttemptedRevision the one that failed when the tool tells.
	SyncFailed SyncPhase = "Failed"
	// SyncSuspended: the sync objects are suspended, nothing is fetched nor
	// applied until they are resumed.
	SyncSuspended SyncPhase = "Suspended"
)

// SyncState is what the sync tool reports about the repository.
type SyncState struct {
	// AppliedRevision is the bare commit SHA whose whole content the tool
	// last applied successfully, in the same form as the revisions of the
	// git store. Adapters strip any tool prefix (Flux reports
	// "main@sha1:<sha>") and report it only from a status the tool wrote for
	// the current spec of its objects, never from a stale one. Empty when
	// nothing was applied yet.
	//
	// The tool may skip commits: a commit is applied when it is an ancestor
	// of AppliedRevision (gitstore.Store.IsAncestor), not only when it is
	// equal to it.
	AppliedRevision string
	// FetchedRevision is the bare commit SHA the tool last fetched, empty when
	// the tool cannot tell. It runs ahead of AppliedRevision while a commit
	// is being applied.
	FetchedRevision string
	// AttemptedRevision is the bare commit SHA whose application failed,
	// set only with SyncFailed and when the tool tells. Empty otherwise.
	AttemptedRevision string
	Phase             SyncPhase
	Message           string
	// ObservedAt is when the adapter read this state.
	ObservedAt time.Time
}

// SyncAdapter wakes the sync tool up and reports what it applied.
type SyncAdapter interface {
	// Validate checks at startup that the sync objects exist and point at the
	// Repository the server commits to. A mismatch is an error: the server
	// would commit to a place nothing applies.
	Validate(ctx context.Context) error
	// RequestSync asks the tool to fetch now instead of at its next interval,
	// after a push of revision was confirmed. Adapters may coalesce requests
	// that arrive close together. It does not wait for the sync.
	//
	// It is best effort: the commit is in git whatever it returns, and the
	// tool applies it at its next interval anyway. A caller never turns a
	// failed RequestSync into a failed write.
	RequestSync(ctx context.Context, revision string) error
	// ObserveSync reads the current state.
	ObserveSync(ctx context.Context) (SyncState, error)
	// WatchSync streams the state, starting with the current one. The channel
	// is closed when ctx ends or when the adapter stops the stream, after
	// which the caller opens a new one.
	WatchSync(ctx context.Context) (<-chan SyncState, error)
}
