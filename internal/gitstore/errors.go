package gitstore

import (
	"errors"
	"fmt"
)

// Errors a store operation wraps. Match them with errors.Is. The package
// does not know the engine vocabulary, its caller maps them.
var (
	// ErrNotFound: the file, directory or revision does not exist.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists: a change required a file to be absent and it exists.
	ErrAlreadyExists = errors.New("already exists")
	// ErrConflict: a file is no longer at the expected revision.
	ErrConflict = errors.New("conflict")
	// ErrIdempotencyConflict: the idempotency key was already used for a
	// different request. It also matches ErrConflict.
	ErrIdempotencyConflict = &sentinel{msg: "idempotency key reused for a different request", is: ErrConflict}
	// ErrInvalidPath: a path or an object name is refused, or the tree has
	// something else than expected at a path (a directory where a file is
	// written, a symlink on the way).
	ErrInvalidPath = errors.New("invalid path")
	// ErrInvalidRequest: a write request is malformed (duplicate or nested
	// paths, contradictory flags, bad key or revision).
	ErrInvalidRequest = errors.New("invalid request")
	// ErrUnavailable: the remote could not be reached before anything was
	// pushed. Nothing was written, the write can be retried as is.
	ErrUnavailable = errors.New("git remote unavailable")
	// ErrRejected: the remote refused the push (hook, protected branch,
	// permissions). Nothing was written.
	ErrRejected = errors.New("push rejected by the git remote")
	// ErrBusy: the branch kept moving under the write until it ran out of
	// attempts. Nothing was written.
	ErrBusy = errors.New("git branch too busy, the write ran out of attempts")
	// ErrUncertain: a push was attempted and its outcome could not be
	// established. The commit may land. The caller reads the state again or
	// replays the write with the same idempotency key.
	ErrUncertain = errors.New("the outcome of the push is unknown")
)

type sentinel struct {
	msg string
	is  error
}

func (s *sentinel) Error() string { return s.msg }

func (s *sentinel) Is(target error) bool { return target == s.is }

// ConflictError details an ErrConflict or ErrAlreadyExists on one file.
type ConflictError struct {
	// Path is relative to the store path.
	Path string
	// Expected is the revision the change expected, empty for a create.
	Expected string
	// Actual is the current revision of the file.
	Actual string
	err    error
}

func (e *ConflictError) Error() string {
	if e.Expected == "" {
		return fmt.Sprintf("%s: %s at revision %s", e.Path, e.err, e.Actual)
	}
	if e.Actual == "" {
		return fmt.Sprintf("%s: %s, expected revision %s, the file no longer exists", e.Path, e.err, e.Expected)
	}
	return fmt.Sprintf("%s: %s, expected revision %s, current revision %s", e.Path, e.err, e.Expected, e.Actual)
}

func (e *ConflictError) Unwrap() error { return e.err }
