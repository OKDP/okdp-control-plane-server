// Package gitstore stores files in one directory of one branch of a git
// remote, with transactional multi-file commits.
//
// Every write fetches the branch, checks its preconditions against the
// fetched head, commits on top of it and pushes without force. When the
// branch moved in between, the write is replayed on the new head, so a
// commit on one file never fails a write on another one. A precondition is
// checked against the revision of the file itself, the last commit that
// changed it, never against the head of the branch.
//
// The repository is held in memory, with the full history of the branch,
// which the revision of a file and the idempotency keys need. Pure Go
// (go-git), no git binary at runtime.
package gitstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
)

const (
	remoteName            = "origin"
	defaultBranch         = "main"
	defaultMaxAttempts    = 10
	defaultVerifyTimeout  = 15 * time.Second
	defaultNetworkTimeout = 2 * time.Minute
)

// DefaultCommitter is the committer of a store configured without one.
var DefaultCommitter = Author{Name: "OKDP Control Plane", Email: "control-plane@okdp.io"}

// Config configures a Store.
type Config struct {
	// Repository is the remote, the branch (main when empty) and the
	// directory the store owns (the repository root when empty). Path is
	// checked, never normalised.
	Repository  gitops.Repository
	Credentials Credentials
	// CredentialsProvider, when set, replaces Credentials and is called
	// before every fetch and push, so that rotated credentials apply
	// without a restart.
	CredentialsProvider func() (Credentials, error)
	// Committer is the identity of the control plane: the committer of every
	// commit, and the author of a write made without a caller identity.
	Committer Author
	// MaxAttempts bounds the fetch, commit, push rounds of one write (10).
	MaxAttempts int
	// VerifyTimeout bounds the fetch that settles a push whose outcome is
	// unclear, when the caller's context is already done (15s).
	VerifyTimeout time.Duration
	// NetworkTimeout bounds each fetch and push on top of the caller's
	// context (2m). The store holds its storage lock meanwhile, so it also
	// bounds how long a read waits for a stalled remote.
	NetworkTimeout time.Duration
}

// Store is a git-backed file store. It is safe for concurrent use. The
// writes of one Store are serialised, two Stores on one remote (two
// replicas) resolve their races through git.
type Store struct {
	cfg      Config
	root     string
	branch   plumbing.ReferenceName
	tracking plumbing.ReferenceName

	// mu guards the go-git storage, which has no locking of its own: a fetch
	// or a commit writes it, every read holds mu until it is done with the
	// objects it reached.
	mu   sync.RWMutex
	repo *git.Repository

	// writeMu serialises the writes and bootstraps of this Store.
	writeMu sync.Mutex
	keys    keyIndex

	// Test seams.
	now        func() time.Time
	beforePush func(attempt int)
	pushFn     func(ctx context.Context, opts *git.PushOptions) error
}

// New checks cfg and prepares a store. It does not reach the remote.
func New(cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.Repository.URL) == "" {
		return nil, errors.New("no git URL")
	}
	if _, err := parseURL(cfg.Repository.URL); err != nil {
		return nil, err
	}
	if cfg.Repository.Branch == "" {
		cfg.Repository.Branch = defaultBranch
	}
	branch := plumbing.NewBranchReferenceName(cfg.Repository.Branch)
	if err := branch.Validate(); err != nil {
		return nil, fmt.Errorf("invalid git branch %q: %w", cfg.Repository.Branch, err)
	}
	root, err := cleanPath(cfg.Repository.Path, true)
	if err != nil {
		return nil, fmt.Errorf("git path: %w", err)
	}
	if cfg.Committer.isZero() {
		cfg.Committer = DefaultCommitter
	}
	if err := validateAuthor(cfg.Committer, "committer"); err != nil {
		return nil, err
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if cfg.VerifyTimeout <= 0 {
		cfg.VerifyTimeout = defaultVerifyTimeout
	}
	if cfg.NetworkTimeout <= 0 {
		cfg.NetworkTimeout = defaultNetworkTimeout
	}

	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		return nil, err
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: remoteName, URLs: []string{cfg.Repository.URL}}); err != nil {
		return nil, err
	}

	s := &Store{
		cfg:      cfg,
		root:     root,
		branch:   branch,
		tracking: plumbing.NewRemoteReferenceName(remoteName, cfg.Repository.Branch),
		repo:     repo,
		keys:     newKeyIndex(),
		now:      time.Now,
	}
	s.pushFn = func(ctx context.Context, opts *git.PushOptions) error {
		return s.repo.PushContext(ctx, opts)
	}
	return s, nil
}

// Repository is the remote, branch and path the store writes to.
func (s *Store) Repository() gitops.Repository {
	r := s.cfg.Repository
	r.Path = s.root
	return r
}

// fetch brings the branch in and returns its head, the zero hash when the
// remote is empty or has no such branch.
func (s *Store) fetch(ctx context.Context) (plumbing.Hash, error) {
	creds, err := s.credentials()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.NetworkTimeout)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()

	refSpec := config.RefSpec(fmt.Sprintf("+%s:%s", s.branch, s.tracking))
	err = s.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: remoteName,
		RefSpecs:   []config.RefSpec{refSpec},
		Auth:       creds.Auth,
		CABundle:   creds.CABundle,
		Tags:       git.NoTags,
		Force:      true,
	})
	switch {
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
	case errors.Is(err, transport.ErrEmptyRemoteRepository), errors.Is(err, git.NoMatchingRefSpecError{}):
		if rerr := s.repo.Storer.RemoveReference(s.tracking); rerr != nil {
			return plumbing.ZeroHash, rerr
		}
		return plumbing.ZeroHash, nil
	default:
		return plumbing.ZeroHash, scrubErrorWithCreds(fmt.Errorf("fetch %s from %s: %w", s.cfg.Repository.Branch, s.cfg.Repository.URL, err), creds)
	}

	ref, err := s.repo.Storer.Reference(s.tracking)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, nil
	}
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return ref.Hash(), nil
}

// errNoCredentials is a CredentialsProvider failure: nothing reached the
// remote.
var errNoCredentials = errors.New("git credentials unavailable")

// credentials returns what the next fetch or push authenticates with.
func (s *Store) credentials() (Credentials, error) {
	if s.cfg.CredentialsProvider == nil {
		return s.cfg.Credentials, nil
	}
	creds, err := s.cfg.CredentialsProvider()
	if err != nil {
		return Credentials{}, fmt.Errorf("%w: %w", errNoCredentials, scrubError(err))
	}
	return creds, nil
}

// Latest fetches the branch and returns a snapshot of its head. On an empty
// remote or a missing branch the snapshot has no revision and every read
// answers ErrNotFound.
func (s *Store) Latest(ctx context.Context) (*Snapshot, error) {
	head, err := s.fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return &Snapshot{s: s, rev: head}, nil
}

// At returns a snapshot of revision, a full commit SHA, fetching the branch
// when the commit is not known yet.
func (s *Store) At(ctx context.Context, revision string) (*Snapshot, error) {
	h, err := parseRevision(revision)
	if err != nil {
		return nil, err
	}
	if !s.hasCommit(h) {
		if _, err := s.fetch(ctx); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		if !s.hasCommit(h) {
			return nil, fmt.Errorf("revision %s: %w", revision, ErrNotFound)
		}
	}
	return &Snapshot{s: s, rev: h}, nil
}

// IsAncestor reports whether ancestor is descendant or one of its ancestors,
// which tells whether an applied revision includes a commit. Both are full
// commit SHAs, fetched when unknown.
func (s *Store) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	a, err := parseRevision(ancestor)
	if err != nil {
		return false, err
	}
	d, err := parseRevision(descendant)
	if err != nil {
		return false, err
	}
	if !s.hasCommit(a) || !s.hasCommit(d) {
		if _, err := s.fetch(ctx); err != nil {
			return false, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range []plumbing.Hash{a, d} {
		if _, err := object.GetCommit(s.repo.Storer, h); err != nil {
			return false, fmt.Errorf("revision %s: %w", h, ErrNotFound)
		}
	}
	return s.isAncestorLocked(a, d)
}

func (s *Store) hasCommit(h plumbing.Hash) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, err := object.GetCommit(s.repo.Storer, h)
	return err == nil
}

// isAncestorLocked walks the history of d. The caller holds mu.
func (s *Store) isAncestorLocked(a, d plumbing.Hash) (bool, error) {
	if a == d {
		return true, nil
	}
	seen := map[plumbing.Hash]bool{d: true}
	queue := []plumbing.Hash{d}
	for len(queue) > 0 {
		c, err := object.GetCommit(s.repo.Storer, queue[0])
		queue = queue[1:]
		if err != nil {
			return false, err
		}
		for _, p := range c.ParentHashes {
			if p == a {
				return true, nil
			}
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return false, nil
}

func parseRevision(rev string) (plumbing.Hash, error) {
	if len(rev) != 40 || !plumbing.IsHash(rev) {
		return plumbing.ZeroHash, fmt.Errorf("%w: %q is not a full commit SHA", ErrInvalidRequest, rev)
	}
	return plumbing.NewHash(rev), nil
}

// EntryKind is the kind of a directory entry.
type EntryKind string

const (
	KindFile EntryKind = "file"
	KindDir  EntryKind = "dir"
	// KindOther is a symlink or a submodule. The store never reads through
	// one nor writes over one.
	KindOther EntryKind = "other"
)

// Entry is one entry of a directory.
type Entry struct {
	Name string
	Kind EntryKind
}

// Snapshot reads the store at one revision. Paths are relative to the store
// path.
type Snapshot struct {
	s   *Store
	rev plumbing.Hash
}

// Revision is the commit SHA of the snapshot, empty for an empty branch.
func (sn *Snapshot) Revision() string {
	if sn.rev.IsZero() {
		return ""
	}
	return sn.rev.String()
}

// ReadFile returns the content of a regular file.
func (sn *Snapshot) ReadFile(p string) ([]byte, error) {
	rel, err := cleanPath(p, false)
	if err != nil {
		return nil, err
	}
	sn.s.mu.RLock()
	defer sn.s.mu.RUnlock()
	st, err := sn.s.stateLocked(sn.rev, join(sn.s.root, rel))
	if err != nil {
		return nil, err
	}
	switch {
	case !st.exists:
		return nil, fmt.Errorf("%s: %w", rel, ErrNotFound)
	case kindOf(st.mode) != KindFile:
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrInvalidPath, rel)
	}
	blob, err := object.GetBlob(sn.s.repo.Storer, st.hash)
	if err != nil {
		return nil, err
	}
	return readBlob(blob)
}

// ReadDir lists a directory, the store path itself for "".
func (sn *Snapshot) ReadDir(p string) ([]Entry, error) {
	rel, err := cleanPath(p, true)
	if err != nil {
		return nil, err
	}
	sn.s.mu.RLock()
	defer sn.s.mu.RUnlock()
	st, err := sn.s.stateLocked(sn.rev, join(sn.s.root, rel))
	if err != nil {
		return nil, err
	}
	switch {
	case !st.exists:
		return nil, fmt.Errorf("%q: %w", rel, ErrNotFound)
	case st.mode != filemode.Dir:
		return nil, fmt.Errorf("%w: %s is not a directory", ErrInvalidPath, rel)
	}
	tree, err := object.GetTree(sn.s.repo.Storer, st.hash)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(tree.Entries))
	for _, e := range tree.Entries {
		entries = append(entries, Entry{Name: e.Name, Kind: kindOf(e.Mode)})
	}
	return entries, nil
}

// Exists reports whether p is a file or a directory at this revision.
func (sn *Snapshot) Exists(p string) (bool, error) {
	rel, err := cleanPath(p, true)
	if err != nil {
		return false, err
	}
	sn.s.mu.RLock()
	defer sn.s.mu.RUnlock()
	st, err := sn.s.stateLocked(sn.rev, join(sn.s.root, rel))
	if err != nil {
		return false, err
	}
	return st.exists, nil
}

// FileRevision returns the revision of p: the last commit, up to this
// snapshot, that changed it. It is what a write passes as ExpectedRevision.
func (sn *Snapshot) FileRevision(p string) (string, error) {
	rel, err := cleanPath(p, true)
	if err != nil {
		return "", err
	}
	sn.s.mu.RLock()
	defer sn.s.mu.RUnlock()
	rev, err := sn.s.revisionLocked(sn.rev, join(sn.s.root, rel))
	if err != nil {
		return "", err
	}
	if rev.IsZero() {
		return "", fmt.Errorf("%q: %w", rel, ErrNotFound)
	}
	return rev.String(), nil
}

// entryState is what a path is in one commit. Two states are equal when the
// path did not change between the two commits.
type entryState struct {
	exists bool
	mode   filemode.FileMode
	hash   plumbing.Hash
}

// stateLocked returns the state of full, a path from the repository root
// ("" for the root itself), in commit. The caller holds mu.
func (s *Store) stateLocked(commit plumbing.Hash, full string) (entryState, error) {
	if commit.IsZero() {
		return entryState{}, nil
	}
	c, err := object.GetCommit(s.repo.Storer, commit)
	if err != nil {
		return entryState{}, err
	}
	return s.stateInTreeLocked(c.TreeHash, full)
}

func (s *Store) stateInTreeLocked(treeHash plumbing.Hash, full string) (entryState, error) {
	if full == "" {
		return entryState{exists: true, mode: filemode.Dir, hash: treeHash}, nil
	}
	current := treeHash
	parts := strings.Split(full, "/")
	for i, part := range parts {
		tree, err := object.GetTree(s.repo.Storer, current)
		if err != nil {
			return entryState{}, err
		}
		var found *object.TreeEntry
		for j := range tree.Entries {
			if tree.Entries[j].Name == part {
				found = &tree.Entries[j]
				break
			}
		}
		if found == nil {
			return entryState{}, nil
		}
		if i == len(parts)-1 {
			return entryState{exists: true, mode: found.Mode, hash: found.Hash}, nil
		}
		if found.Mode != filemode.Dir {
			// A file, symlink or submodule where a directory is expected.
			return entryState{}, nil
		}
		current = found.Hash
	}
	return entryState{}, nil
}

// revisionLocked returns the last commit up to head that changed full, the
// zero hash when full does not exist at head. It follows git's default
// history simplification: from a commit where the path is the same as in
// one of its parents, it goes on with the first such parent, otherwise that
// commit changed the path. A merge that resolved the path differently from
// every parent is the revision. The caller holds mu.
func (s *Store) revisionLocked(head plumbing.Hash, full string) (plumbing.Hash, error) {
	st, err := s.stateLocked(head, full)
	if err != nil || !st.exists {
		return plumbing.ZeroHash, err
	}
	current := head
	for {
		c, err := object.GetCommit(s.repo.Storer, current)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		next := plumbing.ZeroHash
		for _, p := range c.ParentHashes {
			pst, err := s.stateLocked(p, full)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			if pst == st {
				next = p
				break
			}
		}
		if next.IsZero() {
			return current, nil
		}
		current = next
	}
}

func kindOf(m filemode.FileMode) EntryKind {
	switch m {
	case filemode.Regular, filemode.Executable, filemode.Deprecated:
		return KindFile
	case filemode.Dir:
		return KindDir
	default:
		return KindOther
	}
}

func readBlob(b *object.Blob) ([]byte, error) {
	r, err := b.Reader()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(r)
	return data, errors.Join(err, r.Close())
}
