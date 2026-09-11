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
	"sync"
	"unicode"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/sirupsen/logrus"
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
// contract fixes it: "okdp: <action> <project>/<instance> by <user>". The
// logged-in user is also named as co-author, in a trailer Git hosts link to
// their account:
//
//	okdp: deploy demo/trino by alice
//
//	Co-Authored-By: Alice Martin <alice@example.com>
//
// The trailer needs an email: without one (a token lacking the email claim, or
// authentication disabled) it is left out and a warning is logged once per
// user. Name and email are sanitised so a crafted claim cannot add lines.
func CommitMessage(action, target string, actor auth.Actor) string {
	user := stripUnsafe(actor.Username)
	if user == "" {
		user = "unknown"
	}
	subject := fmt.Sprintf("okdp: %s %s by %s", action, target, user)

	email := stripUnsafe(actor.Email)
	if !looksLikeEmail(email) {
		warnNoCoAuthor(user, actor.Email)
		return subject
	}
	name := stripUnsafe(actor.Name)
	if name == "" {
		name = user
	}
	return fmt.Sprintf("%s\n\nCo-Authored-By: %s <%s>", subject, name, email)
}

// stripUnsafe removes what could break out of a trailer line or its <email>
// part: control characters (CR and LF included), '<' and '>'.
func stripUnsafe(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// looksLikeEmail accepts exactly one '@' with something on both sides and no
// white space.
func looksLikeEmail(s string) bool {
	local, domain, ok := strings.Cut(s, "@")
	return ok && local != "" && domain != "" &&
		!strings.Contains(domain, "@") &&
		!strings.ContainsFunc(s, unicode.IsSpace)
}

// warnedActors remembers who was already reported as lacking an email.
var warnedActors sync.Map

func warnNoCoAuthor(user, email string) {
	if _, seen := warnedActors.LoadOrStore(user, struct{}{}); seen {
		return
	}
	reason := "the token has no email claim (Keycloak: add the \"email\" client scope)"
	switch {
	case email != "":
		reason = "its email claim is not a valid address"
	case user == "anonymous":
		reason = "no logged-in user (authentication disabled)"
	}
	logrus.Warnf("gitops: commits by %q carry no Co-Authored-By trailer: %s", user, reason)
}
