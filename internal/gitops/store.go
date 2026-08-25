// Package gitops holds the desired state of the platform: files in a Git
// repository, the only store the server has. The cluster is never written to
// directly; a GitOps engine (Flux or Argo CD) reconciles what is committed here.
//
// The package is layered:
//   - Store is a transactional view of a Git working tree (GitStore talks to a
//     real remote through go-git, MemoryStore backs the tests);
//   - Deployments knows the layout of the deployments repository (the shared
//     no-kubocd contract) and writes exactly its files;
//   - FluxRenderer produces the Flux-only files from the engine-neutral ones.
package gitops

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
)

// ErrNotFound is returned when a desired-state file does not exist.
var ErrNotFound = errors.New("not found in the deployments repository")

// ErrExists is returned when creating something that is already declared.
var ErrExists = errors.New("already exists in the deployments repository")

// ErrConflict is returned when a write kept losing the race against other
// writers of the same branch, after every retry.
var ErrConflict = errors.New("the deployments repository kept changing under the write, giving up")

// Reader reads files of the desired state. Paths are slash-separated and
// relative to the configured GITOPS_PATH prefix.
type Reader interface {
	// ReadFile returns the content of a file, or an error wrapping ErrNotFound.
	ReadFile(p string) ([]byte, error)
	// ReadDir returns the sorted names of the entries of a directory, nil when
	// the directory does not exist.
	ReadDir(p string) ([]string, error)
	// Exists reports whether a file or a directory exists.
	Exists(p string) bool
}

// Tx is a write transaction on the desired state. Its changes are committed
// together, or not at all.
type Tx interface {
	Reader
	WriteFile(p string, data []byte) error
	// Remove deletes a file or a whole directory. Removing nothing is not an error.
	Remove(p string) error
}

// Store is the desired-state store.
type Store interface {
	// View runs fn against a consistent, recently refreshed snapshot.
	View(ctx context.Context, fn func(r Reader) error) error
	// Update runs fn on top of the latest remote revision and commits its
	// changes with message. When another writer pushed in between, the change
	// is replayed on the new revision (the equivalent of a rebase: fn is the
	// change, not a patch) and pushed again. A transaction changing nothing
	// commits nothing. Returns the revision holding the change.
	Update(ctx context.Context, message string, fn func(tx Tx) error) (string, error)
}

// cleanPath validates a repository-relative path. Absolute paths and paths
// escaping the prefix are refused: every path is built by this package from
// validated names, so one reaching here is a bug worth failing on.
func cleanPath(p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("absolute path %q in the deployments repository", p)
	}
	cleaned := path.Clean(p)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q escapes the deployments repository", p)
	}
	return cleaned, nil
}

// CommitMessage builds the commit message of a console action, as the shared
// contract fixes it: "okdp: <action> <project>/<instance> by <user>".
func CommitMessage(action, target, user string) string {
	if user == "" {
		user = "unknown"
	}
	return fmt.Sprintf("okdp: %s %s by %s", action, target, user)
}
