package gitstore

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// Change is one file of a write. Path is relative to the store path.
type Change struct {
	Path string
	// Content is the new content of the file, ignored with Delete.
	Content []byte
	Delete  bool
	// DeleteTree removes the directory at Path with everything under it,
	// whatever it holds when the write lands. The directory must exist.
	DeleteTree bool
	// MustNotExist makes the write fail with ErrAlreadyExists when the file
	// exists: a create.
	MustNotExist bool
	// ExpectedRevision, when set, makes the write fail with ErrConflict
	// unless the file exists and its revision (Snapshot.FileRevision) is
	// still this one.
	ExpectedRevision string
}

// WriteRequest is one atomic write: all its changes land in one commit, or
// none does.
type WriteRequest struct {
	Changes []Change
	// Author is who asked for the write, see AuthorFromIdentity. The store
	// committer when zero.
	Author  Author
	Message string
	// IdempotencyKey, when set, makes a replay of the same request answer the
	// result of the first one, and a different request with the same key
	// fail with ErrIdempotencyConflict. The key is recorded in the commit and
	// looked up in the whole history of the branch, so it survives restarts
	// and is shared by replicas. It is scoped to the branch: a caller that
	// lets users pick keys prefixes them.
	IdempotencyKey string
}

// WriteResult is the outcome of a write that did not fail.
type WriteResult struct {
	// Revision is the commit holding the write: the new commit, the first
	// one for a replay, the head for an unchanged write.
	Revision string
	// Files gives the revision of each changed file after the write, to pass
	// as its next ExpectedRevision, empty for a deleted file. It is not
	// Revision for a file the write left as it was.
	Files map[string]string
	// Replayed reports a replay answered from the first write.
	Replayed bool
	// Unchanged reports a write that changed nothing and made no commit.
	// Never set with an idempotency key, which is always recorded.
	Unchanged bool
}

const (
	trailerKey    = "Okdp-Idempotency-Key"
	trailerDigest = "Okdp-Request-Digest"
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// Write commits the changes of req as one commit on the branch. See the
// package documentation for how it survives concurrent writers.
func (s *Store) Write(ctx context.Context, req WriteRequest) (*WriteResult, error) {
	changes, err := s.checkRequest(req)
	if err != nil {
		return nil, err
	}
	digest := requestDigest(s.root, changes)
	author := req.Author
	if author.isZero() {
		author = s.cfg.Committer
	}
	message := commitMessage(req.Message, changes, req.IdempotencyKey, digest)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	plan := func(head plumbing.Hash) (planned, error) {
		if req.IdempotencyKey != "" {
			rec, found, err := s.lookupKey(head, req.IdempotencyKey)
			if err != nil {
				return planned{}, err
			}
			if found {
				if rec.digest != digest {
					return planned{}, fmt.Errorf("%w: key %q, first used by %s", ErrIdempotencyConflict, req.IdempotencyKey, rec.commit)
				}
				return planned{done: true, commit: rec.commit, replayed: true}, nil
			}
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.checkPreconditionsLocked(head, changes); err != nil {
			return planned{}, err
		}
		base := plumbing.ZeroHash
		if !head.IsZero() {
			c, err := object.GetCommit(s.repo.Storer, head)
			if err != nil {
				return planned{}, err
			}
			base = c.TreeHash
		}
		ops := make(map[string]treeOp, len(changes))
		for _, c := range changes {
			ops[join(s.root, c.Path)] = treeOp{content: c.Content, delete: c.Delete, deleteTree: c.DeleteTree}
		}
		tree, err := s.applyLocked(base, ops)
		if err != nil {
			return planned{}, err
		}
		if tree, err = s.keepRootLocked(tree); err != nil {
			return planned{}, err
		}
		if tree == base && !head.IsZero() && req.IdempotencyKey == "" {
			return planned{done: true, commit: head, unchanged: true}, nil
		}
		return planned{tree: tree, author: author, message: message}, nil
	}

	out, err := s.commitLoop(ctx, plan)
	if err != nil {
		return nil, err
	}
	files, err := s.fileRevisions(out.commit, changes)
	if err != nil {
		return nil, err
	}
	return &WriteResult{Revision: out.commit.String(), Files: files, Replayed: out.replayed, Unchanged: out.unchanged}, nil
}

// keepRootLocked adds <path>/.gitkeep to a tree the changes left without the
// store directory: git drops an empty directory, and the sync tool fails on
// a path that does not exist. The repository root always exists. The
// caller holds mu for writing.
func (s *Store) keepRootLocked(tree plumbing.Hash) (plumbing.Hash, error) {
	if s.root == "" {
		return tree, nil
	}
	st, err := s.stateInTreeLocked(tree, s.root)
	if err != nil || st.exists {
		return tree, err
	}
	return s.applyLocked(tree, map[string]treeOp{join(s.root, placeholder): {content: []byte{}}})
}

// checkRequest validates a request before anything is fetched and returns
// its changes sorted by path.
func (s *Store) checkRequest(req WriteRequest) ([]Change, error) {
	if len(req.Changes) == 0 {
		return nil, fmt.Errorf("%w: no change", ErrInvalidRequest)
	}
	if req.IdempotencyKey != "" && !idempotencyKeyPattern.MatchString(req.IdempotencyKey) {
		return nil, fmt.Errorf("%w: idempotency key %q must match %s", ErrInvalidRequest, req.IdempotencyKey, idempotencyKeyPattern)
	}
	lower := strings.ToLower(req.Message)
	for _, t := range []string{trailerKey, trailerDigest} {
		if strings.Contains(lower, strings.ToLower(t)) {
			return nil, fmt.Errorf("%w: the message must not carry %s", ErrInvalidRequest, t)
		}
	}
	if strings.ContainsRune(req.Message, 0) {
		return nil, fmt.Errorf("%w: the message holds a NUL byte", ErrInvalidRequest)
	}
	if !req.Author.isZero() {
		if err := validateAuthor(req.Author, "author"); err != nil {
			return nil, err
		}
	}

	changes := make([]Change, len(req.Changes))
	copy(changes, req.Changes)
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	for i, c := range changes {
		if _, err := cleanPath(c.Path, false); err != nil {
			return nil, err
		}
		switch {
		case c.DeleteTree && (c.Delete || len(c.Content) > 0 || c.MustNotExist || c.ExpectedRevision != ""):
			return nil, fmt.Errorf("%w: %s is a tree deletion with another flag or a content", ErrInvalidRequest, c.Path)
		case c.Delete && len(c.Content) > 0:
			return nil, fmt.Errorf("%w: %s is deleted with a content", ErrInvalidRequest, c.Path)
		case c.Delete && c.MustNotExist:
			return nil, fmt.Errorf("%w: %s is deleted and must not exist", ErrInvalidRequest, c.Path)
		case c.MustNotExist && c.ExpectedRevision != "":
			return nil, fmt.Errorf("%w: %s must not exist and has an expected revision", ErrInvalidRequest, c.Path)
		}
		if c.ExpectedRevision != "" {
			if _, err := parseRevision(c.ExpectedRevision); err != nil {
				return nil, fmt.Errorf("%s: %w", c.Path, err)
			}
		}
		if i > 0 {
			prev := changes[i-1].Path
			if prev == c.Path {
				return nil, fmt.Errorf("%w: %s is changed twice", ErrInvalidRequest, c.Path)
			}
		}
	}
	// Sorted, a path is followed by its descendants, but not necessarily
	// right after it ("a/b", "a/b.yaml", "a/b/c"): compare each pair.
	for i := range changes {
		for j := i + 1; j < len(changes); j++ {
			if strings.HasPrefix(changes[j].Path, changes[i].Path+"/") {
				return nil, fmt.Errorf("%w: %s is inside %s, changed by the same write", ErrInvalidRequest, changes[j].Path, changes[i].Path)
			}
		}
	}
	return changes, nil
}

// checkPreconditionsLocked checks each change against head. The caller
// holds mu.
func (s *Store) checkPreconditionsLocked(head plumbing.Hash, changes []Change) error {
	for _, c := range changes {
		full := join(s.root, c.Path)
		st, err := s.stateLocked(head, full)
		if err != nil {
			return err
		}
		if c.DeleteTree {
			switch {
			case !st.exists:
				return fmt.Errorf("%s: %w", c.Path, ErrNotFound)
			case st.mode != filemode.Dir:
				return fmt.Errorf("%w: %s is a %s, not a directory", ErrInvalidPath, c.Path, kindOf(st.mode))
			}
			continue
		}
		if st.exists && kindOf(st.mode) != KindFile {
			return fmt.Errorf("%w: %s is a %s, not a regular file", ErrInvalidPath, c.Path, kindOf(st.mode))
		}
		var current string
		if st.exists && (c.MustNotExist || c.ExpectedRevision != "") {
			rev, err := s.revisionLocked(head, full)
			if err != nil {
				return err
			}
			current = rev.String()
		}
		switch {
		case c.MustNotExist && st.exists:
			return &ConflictError{Path: c.Path, Actual: current, err: ErrAlreadyExists}
		case c.ExpectedRevision != "" && c.ExpectedRevision != current:
			return &ConflictError{Path: c.Path, Expected: c.ExpectedRevision, Actual: current, err: ErrConflict}
		case c.Delete && !st.exists:
			return fmt.Errorf("%s: %w", c.Path, ErrNotFound)
		}
	}
	return nil
}

func (s *Store) fileRevisions(commit plumbing.Hash, changes []Change) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	files := make(map[string]string, len(changes))
	for _, c := range changes {
		rev, err := s.revisionLocked(commit, join(s.root, c.Path))
		if err != nil {
			return nil, err
		}
		if rev.IsZero() {
			files[c.Path] = ""
		} else {
			files[c.Path] = rev.String()
		}
	}
	return files, nil
}

// requestDigest identifies a request for its idempotency key: the changes,
// sorted, with their path from the repository root (two stores rooted at
// different paths of one branch never share a digest), their
// preconditions and a hash of their content.
func requestDigest(root string, changes []Change) string {
	h := newDigest()
	h.line("okdp-gitstore-request-v1")
	for _, c := range changes {
		op := "write"
		switch {
		case c.Delete:
			op = "delete"
		case c.DeleteTree:
			op = "delete-tree"
		}
		h.line(join(root, c.Path), op, fmt.Sprint(c.MustNotExist), c.ExpectedRevision, h.sum(c.Content))
	}
	return "sha256:" + h.hex()
}

func commitMessage(message string, changes []Change, key, digest string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		if len(changes) == 1 {
			message = "Update " + changes[0].Path
		} else {
			message = fmt.Sprintf("Update %d files", len(changes))
		}
	}
	if key != "" {
		message += fmt.Sprintf("\n\n%s: %s\n%s: %s", trailerKey, key, trailerDigest, digest)
	}
	return message + "\n"
}

// planned is what a planner decided against one head: either the write is
// already done (done, commit) or it needs a commit of tree.
type planned struct {
	done      bool
	commit    plumbing.Hash
	replayed  bool
	unchanged bool

	tree    plumbing.Hash
	author  Author
	message string
}

// attempt is one pushed commit, kept until the write ends.
type attempt struct {
	commit, parent plumbing.Hash
	// ambiguous: the push failed without saying whether the remote took it.
	ambiguous bool
}

// loopState is what a write learnt about its pushes.
type loopState struct {
	attempts []attempt
	// head is the last head fetched, valid once seen is set.
	head plumbing.Hash
	seen bool
	// rewound: the branch went back or was deleted during the write, so a
	// moved head no longer proves that a push is lost.
	rewound bool
}

// pending reports whether an ambiguous push may still land on the branch
// at the last head. The remote updates the branch only if it is still at
// the parent of the push: once the head moved away from that parent, the
// push is lost, unless the branch can go back.
func (st *loopState) pending() bool {
	for _, a := range st.attempts {
		if a.ambiguous && (st.rewound || !st.seen || a.parent == st.head) {
			return true
		}
	}
	return false
}

// observe records a fetched head and whether it rewound the branch.
func (s *Store) observe(st *loopState, head plumbing.Hash) error {
	if st.seen && !st.rewound && !st.head.IsZero() && st.head != head {
		if head.IsZero() {
			st.rewound = true
		} else {
			s.mu.RLock()
			ok, err := s.isAncestorLocked(st.head, head)
			s.mu.RUnlock()
			if err != nil {
				return err
			}
			st.rewound = !ok
		}
	}
	st.head, st.seen = head, true
	return nil
}

// commitLoop fetches, plans against the head, commits and pushes until the
// write lands, is refused, or runs out of attempts.
//
// The store assumes the branch is never rewound nor deleted (force push),
// which a GitOps branch is protected against. A failed push is classified
// by the state of the remote, read by the next fetch, and by the errors
// that prove nothing was applied:
//   - a commit we pushed is in the branch: success, whatever the error said.
//   - the remote refused the push for a policy reason (hook, protected
//     branch, credentials): ErrRejected, without retry.
//   - the branch was not where the push expected it: the write is replayed
//     on the new head.
//   - the remote did not apply the push for a reason it does not name:
//     replayed if the branch moved, ErrRejected otherwise.
//   - the push failed in transit: pushed again from the new head while
//     attempts and the context last. The write ends with ErrUncertain as
//     long as such a push may still land.
func (s *Store) commitLoop(ctx context.Context, plan func(head plumbing.Hash) (planned, error)) (planned, error) {
	var (
		st      loopState
		lastErr error
		// rejected is a pushRejected error, with the head it was pushed on.
		rejected     error
		rejectedHead plumbing.Hash
	)
	for round := 0; ; round++ {
		fetchCtx, cancel := ctx, context.CancelFunc(func() {})
		if st.pending() {
			fetchCtx, cancel = context.WithTimeout(ctx, s.cfg.VerifyTimeout)
		}
		head, err := s.fetch(fetchCtx)
		cancel()
		if err != nil {
			return s.settle(&st, fmt.Errorf("%w: %w", ErrUnavailable, err))
		}
		if err := s.observe(&st, head); err != nil {
			return planned{}, err
		}
		if out, ok, err := s.landed(head, st.attempts); err != nil || ok {
			return out, err
		}
		if rejected != nil && head == rejectedHead {
			return s.settle(&st, fmt.Errorf("%w: %w", ErrRejected, rejected))
		}
		rejected = nil
		if err := ctx.Err(); err != nil {
			return s.settle(&st, fmt.Errorf("%w: %w", ErrUnavailable, err))
		}
		if round >= s.cfg.MaxAttempts {
			return s.settle(&st, fmt.Errorf("%w: last push error: %v", ErrBusy, lastErr))
		}

		p, err := plan(head)
		if err != nil {
			if st.pending() {
				return planned{}, fmt.Errorf("%w: an earlier push may still land: %v", ErrUncertain, err)
			}
			return planned{}, err
		}
		if p.done {
			return p, nil
		}

		commit, err := s.commit(head, p)
		if err != nil {
			return planned{}, err
		}
		if s.beforePush != nil {
			s.beforePush(round)
		}
		kind, perr := s.push(ctx, head)
		if perr == nil {
			return planned{commit: commit}, nil
		}
		lastErr = perr
		switch kind {
		case pushRefused:
			return s.settle(&st, fmt.Errorf("%w: %w", ErrRejected, perr))
		case pushRejected:
			rejected, rejectedHead = perr, head
		case pushAmbiguous:
			st.attempts = append(st.attempts, attempt{commit: commit, parent: head, ambiguous: true})
		case pushLost:
		}
		if err := sleep(ctx, backoff(round)); err != nil {
			return s.settle(&st, fmt.Errorf("%w: %w", ErrUnavailable, err))
		}
	}
}

// commit stores the commit of p on head and points the local branch at it.
func (s *Store) commit(head plumbing.Hash, p planned) (plumbing.Hash, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	c := &object.Commit{
		Author:    object.Signature{Name: p.author.Name, Email: p.author.Email, When: now},
		Committer: object.Signature{Name: s.cfg.Committer.Name, Email: s.cfg.Committer.Email, When: now},
		Message:   p.message,
		TreeHash:  p.tree,
	}
	if !head.IsZero() {
		c.ParentHashes = []plumbing.Hash{head}
	}
	h, err := s.storeCommitLocked(c)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if err := s.repo.Storer.SetReference(plumbing.NewHashReference(s.branch, h)); err != nil {
		return plumbing.ZeroHash, err
	}
	return h, nil
}

// push sends the local branch, requiring the remote branch to still be at
// head, absent when head is zero. Errors are classified before scrubbing so
// sentinel matching still works while active credentials never leak.
func (s *Store) push(ctx context.Context, head plumbing.Hash) (pushKind, error) {
	creds, err := s.credentials()
	if err != nil {
		return classifyPush(err), scrubError(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	opts := &git.PushOptions{
		RemoteName: remoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec(fmt.Sprintf("%s:%s", s.branch, s.branch))},
		Auth:       creds.Auth,
		CABundle:   creds.CABundle,
	}
	if !head.IsZero() {
		opts.RequireRemoteRefs = []config.RefSpec{config.RefSpec(fmt.Sprintf("%s:%s", head, s.branch))}
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.NetworkTimeout)
	defer cancel()
	err = s.pushFn(ctx, opts)
	if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
		// Only possible when an earlier push of this very commit landed.
		return 0, nil
	}
	kind := classifyPush(err)
	return kind, scrubErrorWithCreds(err, creds)
}

type pushKind int

const (
	// pushLost: not applied because the remote branch was not where the push
	// expected it, the write is replayed.
	pushLost pushKind = iota
	// pushRefused: not applied for a reason a replay does not change.
	pushRefused
	// pushRejected: the remote did not apply it, for a reason the store
	// does not know. Lost if the branch moved, refused otherwise.
	pushRejected
	// pushAmbiguous: anything else, the remote may have taken it.
	pushAmbiguous
)

// Statuses git receive-pack reports for a branch that moved under a push,
// and for a policy refusal.
var (
	raceStatuses = []string{
		"incorrect old value", "cannot lock ref", "failed to update ref", "failed to lock",
		"reference already exists", "non-fast-forward", "fetch first", "stale info",
	}
	policyStatuses = []string{"hook declined", "denied", "deny", "protected", "not allowed", "permission", "refusing"}
)

// classifyPush only tells apart the failures that prove nothing was
// applied. go-git reports the remote's answer and its own refusals with
// plain formatted errors, matched here on its wording.
func classifyPush(err error) pushKind {
	for _, refused := range []error{
		errNoCredentials,
		transport.ErrAuthenticationRequired, transport.ErrAuthorizationFailed,
		transport.ErrRepositoryNotFound, transport.ErrInvalidAuthMethod,
	} {
		if errors.Is(err, refused) {
			return pushRefused
		}
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "command error on "):
		// "command error on <ref>: <status>". A ref name cannot hold a
		// colon, so the status is what follows the first one.
		_, status, _ := strings.Cut(strings.TrimPrefix(msg, "command error on "), ": ")
		lowerStatus := strings.ToLower(status)
		switch {
		case containsAny(lowerStatus, policyStatuses):
			return pushRefused
		case containsAny(lowerStatus, raceStatuses):
			return pushLost
		default:
			return pushRejected
		}
	case strings.HasPrefix(msg, "unpack error"):
		return pushRefused
	case strings.HasPrefix(msg, "remote ref ") && strings.Contains(msg, " required to be "),
		strings.HasPrefix(msg, "non-fast-forward update: "):
		return pushLost
	default:
		return pushAmbiguous
	}
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// landed reports whether one of the attempts is in the branch at head.
func (s *Store) landed(head plumbing.Hash, attempts []attempt) (planned, bool, error) {
	if head.IsZero() || len(attempts) == 0 {
		return planned{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range attempts {
		ok, err := s.isAncestorLocked(a.commit, head)
		if err != nil {
			return planned{}, false, err
		}
		if ok {
			return planned{commit: a.commit}, true, nil
		}
	}
	return planned{}, false, nil
}

// settle ends a write that could not complete. When a push may still land,
// it fetches once more, on a fresh context bounded by VerifyTimeout, and
// answers ErrUncertain unless that fetch proves the push landed or lost.
// An uncertain error never matches the sentinels of a certain failure, its
// cause is only quoted.
func (s *Store) settle(st *loopState, cause error) (planned, error) {
	if !st.pending() {
		return planned{}, cause
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.VerifyTimeout)
	defer cancel()
	head, err := s.fetch(ctx)
	if err != nil {
		return planned{}, fmt.Errorf("%w: %v (verification: %v)", ErrUncertain, cause, err)
	}
	if err := s.observe(st, head); err != nil {
		return planned{}, fmt.Errorf("%w: %v (verification: %v)", ErrUncertain, cause, err)
	}
	if out, ok, err := s.landed(head, st.attempts); err != nil || ok {
		return out, err
	}
	if st.pending() {
		return planned{}, fmt.Errorf("%w: %v", ErrUncertain, cause)
	}
	return planned{}, cause
}

func backoff(round int) time.Duration {
	base := 20 * time.Millisecond << min(round, 5)
	return base/2 + rand.N(base)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
