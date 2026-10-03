package gitops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/sirupsen/logrus"
)

// GitOptions configures a GitStore.
type GitOptions struct {
	// URL of the deployments repository (https://, ssh://, git@host:path or file://).
	URL string
	// Branch the desired state lives on.
	Branch string
	// PathPrefix is the directory of the repository holding the layout
	// (GITOPS_PATH), empty for the root.
	PathPrefix string
	// Auth authenticates the fetches and pushes, nil for anonymous access.
	Auth transport.AuthMethod
	// AuthorName and AuthorEmail are the service identity: the committer of
	// every commit, and its author when the commit names no user
	// (GITOPS_AUTHOR_NAME, GITOPS_AUTHOR_EMAIL).
	AuthorName  string
	AuthorEmail string
	// CloneDir holds the local clone, a cache only (an emptyDir in the pod).
	// Empty keeps it in memory.
	CloneDir string
	// MaxRetries bounds the replays of a write that lost a race.
	MaxRetries int
	// RefreshInterval is how stale a read may be before it fetches again.
	RefreshInterval time.Duration
}

// GitStore is a Store backed by a Git remote through go-git. Every operation
// is serialised: a write fetches, applies, commits and pushes as one step.
type GitStore struct {
	opts GitOptions

	mu       sync.Mutex
	repo     *git.Repository
	worktree *git.Worktree
	root     billy.Filesystem
	lastSync time.Time
	synced   bool
}

// NewGitStore prepares the local clone. It does not reach the remote: the
// first read or write does, so a server can start while Git is unreachable.
func NewGitStore(opts GitOptions) (*GitStore, error) {
	if opts.URL == "" {
		return nil, errors.New("the deployments repository URL is required")
	}
	if opts.Branch == "" {
		opts.Branch = "main"
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 5
	}
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = 5 * time.Second
	}
	if opts.AuthorName == "" {
		opts.AuthorName = DefaultCommitter.Name
	}
	if opts.AuthorEmail == "" {
		opts.AuthorEmail = DefaultCommitter.Email
	}
	prefix, err := cleanPath(strings.Trim(opts.PathPrefix, "/"))
	if err != nil {
		return nil, err
	}
	opts.PathPrefix = prefix

	var repo *git.Repository
	if opts.CloneDir == "" {
		repo, err = git.Init(memory.NewStorage(), memfs.New())
	} else {
		repo, err = git.PlainOpen(opts.CloneDir)
		if errors.Is(err, git.ErrRepositoryNotExists) {
			if mkErr := os.MkdirAll(opts.CloneDir, 0o755); mkErr != nil {
				return nil, mkErr
			}
			repo, err = git.PlainInit(opts.CloneDir, false)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to prepare the local clone: %w", err)
	}

	// The remote is (re)declared on every start: the URL may have changed
	// since the clone directory was last used.
	_ = repo.DeleteRemote("origin")
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{opts.URL}}); err != nil {
		return nil, fmt.Errorf("failed to declare the remote: %w", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err
	}
	root := worktree.Filesystem
	if prefix != "." {
		root, err = worktree.Filesystem.Chroot(prefix)
		if err != nil {
			return nil, err
		}
	}

	return &GitStore{opts: opts, repo: repo, worktree: worktree, root: root}, nil
}

func (s *GitStore) branchRef() plumbing.ReferenceName {
	return plumbing.NewBranchReferenceName(s.opts.Branch)
}

func (s *GitStore) remoteRef() plumbing.ReferenceName {
	return plumbing.NewRemoteReferenceName("origin", s.opts.Branch)
}

// sync makes the working tree exactly the remote branch, dropping whatever a
// failed write left behind. An empty remote (or a missing branch) is a valid,
// empty desired state: the first write creates the branch.
func (s *GitStore) sync(ctx context.Context) error {
	// Forget unpushed local commits first: they are either pushed already or
	// about to be replayed, and advertising them as "haves" confuses some
	// servers into failing the fetch.
	if ref, err := s.repo.Reference(s.remoteRef(), true); err == nil {
		_ = s.repo.Storer.SetReference(plumbing.NewHashReference(s.branchRef(), ref.Hash()))
	} else {
		_ = s.repo.Storer.RemoveReference(s.branchRef())
	}

	refSpec := config.RefSpec(fmt.Sprintf("+%s:%s", s.branchRef(), s.remoteRef()))
	err := s.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{refSpec},
		Auth:       s.opts.Auth,
		Force:      true,
	})
	remoteMissing := false
	switch {
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
	case errors.Is(err, transport.ErrEmptyRemoteRepository), errors.Is(err, git.NoMatchingRefSpecError{}):
		remoteMissing = true
	default:
		return fmt.Errorf("failed to fetch %s from the deployments repository: %w", s.opts.Branch, err)
	}

	if err := s.repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, s.branchRef())); err != nil {
		return err
	}

	if !remoteMissing {
		ref, err := s.repo.Reference(s.remoteRef(), true)
		if err != nil {
			return fmt.Errorf("failed to resolve the fetched branch: %w", err)
		}
		if err := s.repo.Storer.SetReference(plumbing.NewHashReference(s.branchRef(), ref.Hash())); err != nil {
			return err
		}
		if err := s.worktree.Reset(&git.ResetOptions{Commit: ref.Hash(), Mode: git.HardReset}); err != nil {
			return fmt.Errorf("failed to reset the local clone: %w", err)
		}
	} else {
		// Nothing upstream: forget any local commit and start from an empty tree.
		_ = s.repo.Storer.RemoveReference(s.branchRef())
		if err := s.repo.Storer.SetIndex(&index.Index{Version: 2}); err != nil {
			return err
		}
	}
	if err := s.worktree.Clean(&git.CleanOptions{Dir: true}); err != nil {
		return fmt.Errorf("failed to clean the local clone: %w", err)
	}

	s.lastSync = time.Now()
	s.synced = true
	return nil
}

// refresh syncs when the local clone is older than the refresh interval. A
// failed refresh keeps serving the last known state, with a warning, rather
// than failing every page while Git is briefly unreachable.
func (s *GitStore) refresh(ctx context.Context) error {
	if s.synced && time.Since(s.lastSync) < s.opts.RefreshInterval {
		return nil
	}
	if err := s.sync(ctx); err != nil {
		if s.synced {
			logrus.WithError(err).Warn("Could not refresh the deployments repository, serving the last known state")
			return nil
		}
		return err
	}
	return nil
}

func (s *GitStore) View(ctx context.Context, fn func(r Reader) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(ctx); err != nil {
		return err
	}
	return fn(&fsTx{fs: s.root})
}

func (s *GitStore) Update(ctx context.Context, commit Commit, fn func(tx Tx) error) (string, error) {
	committerID := Signature{Name: s.opts.AuthorName, Email: s.opts.AuthorEmail}
	authorID := committerID
	if commit.Author != nil {
		authorID = *commit.Author
	}
	message := commit.Message(committerID)

	s.mu.Lock()
	defer s.mu.Unlock()

	for attempt := 0; attempt <= s.opts.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := s.sync(ctx); err != nil {
			return "", err
		}
		if err := fn(&fsTx{fs: s.root}); err != nil {
			// The working tree may be half-written: the next operation syncs
			// before anything else reads it.
			s.synced = false
			return "", err
		}

		if err := s.worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
			s.synced = false
			return "", fmt.Errorf("failed to stage the change: %w", err)
		}
		status, err := s.worktree.Status()
		if err != nil {
			s.synced = false
			return "", err
		}
		if status.IsClean() {
			head, err := s.repo.Head()
			if err != nil {
				return "", nil
			}
			return head.Hash().String(), nil
		}

		now := time.Now()
		hash, err := s.worktree.Commit(message, &git.CommitOptions{
			Author:    &object.Signature{Name: authorID.Name, Email: authorID.Email, When: now},
			Committer: &object.Signature{Name: committerID.Name, Email: committerID.Email, When: now},
		})
		if err != nil {
			s.synced = false
			return "", fmt.Errorf("failed to commit: %w", err)
		}

		err = s.repo.PushContext(ctx, &git.PushOptions{
			RemoteName: "origin",
			RefSpecs:   []config.RefSpec{config.RefSpec(fmt.Sprintf("%s:%s", s.branchRef(), s.branchRef()))},
			Auth:       s.opts.Auth,
		})
		if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
			// The remote now holds exactly this commit.
			_ = s.repo.Storer.SetReference(plumbing.NewHashReference(s.remoteRef(), hash))
			s.lastSync = now
			logrus.WithField("revision", hash.String()).WithField("author", authorID.Name).WithField("message", commit.Subject).Info("Committed to the deployments repository")
			return hash.String(), nil
		}
		s.synced = false
		if isNonFastForward(err) {
			logrus.WithError(err).WithField("attempt", attempt+1).Info("The deployments repository moved during the write, replaying it on the new revision")
			continue
		}
		return "", fmt.Errorf("failed to push to the deployments repository: %w", err)
	}
	return "", ErrConflict
}

// isNonFastForward recognises a push refused because the branch moved. go-git
// detects it itself when it sees the remote ref first; a server may also
// refuse it, and only says so in words.
func isNonFastForward(err error) bool {
	if errors.Is(err, git.ErrNonFastForwardUpdate) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"non-fast-forward", "fetch first", "reference has changed concurrently", "failed to lock", "cannot lock ref"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// fsTx is a Tx over a billy filesystem rooted at the path prefix.
type fsTx struct {
	fs billy.Filesystem
}

func (t *fsTx) ReadFile(p string) ([]byte, error) {
	cleaned, err := cleanPath(p)
	if err != nil {
		return nil, err
	}
	data, err := util.ReadFile(t.fs, cleaned)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", cleaned, ErrNotFound)
	}
	return data, err
}

func (t *fsTx) ReadDir(p string) ([]string, error) {
	cleaned, err := cleanPath(p)
	if err != nil {
		return nil, err
	}
	entries, err := t.fs.ReadDir(cleaned)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

func (t *fsTx) Exists(p string) bool {
	cleaned, err := cleanPath(p)
	if err != nil {
		return false
	}
	_, err = t.fs.Stat(cleaned)
	return err == nil
}

func (t *fsTx) WriteFile(p string, data []byte) error {
	cleaned, err := cleanPath(p)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(cleaned); dir != "." {
		if err := t.fs.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return util.WriteFile(t.fs, cleaned, data, 0o644)
}

func (t *fsTx) Remove(p string) error {
	cleaned, err := cleanPath(p)
	if err != nil {
		return err
	}
	if cleaned == "." {
		return errors.New("refusing to remove the whole deployments tree")
	}
	if !t.Exists(cleaned) {
		return nil
	}
	return util.RemoveAll(t.fs, cleaned)
}
