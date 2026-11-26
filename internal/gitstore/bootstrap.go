package gitstore

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// placeholder keeps the store directory in git while it holds nothing else.
const placeholder = ".gitkeep"

// BootstrapResult tells what Bootstrap found.
type BootstrapResult struct {
	// Revision is the head of the branch after Bootstrap.
	Revision string
	// Created reports that Bootstrap committed the store directory.
	Created bool
}

// Bootstrap makes sure the branch and the store directory exist. On an
// empty remote, a missing branch or a missing directory it commits
// <path>/.gitkeep, a root commit when the branch does not exist. It never
// forces a push nor touches existing content, and a concurrent Bootstrap
// (another replica) makes it replay and find the directory there.
func (s *Store) Bootstrap(ctx context.Context) (BootstrapResult, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	full := join(s.root, placeholder)
	plan := func(head plumbing.Hash) (planned, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		base := plumbing.ZeroHash
		if !head.IsZero() {
			st, err := s.stateLocked(head, s.root)
			if err != nil {
				return planned{}, err
			}
			switch {
			case st.exists && st.mode == filemode.Dir:
				return planned{done: true, commit: head, unchanged: true}, nil
			case st.exists:
				return planned{}, fmt.Errorf("%w: the store path %q is a %s in the branch", ErrInvalidPath, s.root, kindOf(st.mode))
			}
			c, err := object.GetCommit(s.repo.Storer, head)
			if err != nil {
				return planned{}, err
			}
			base = c.TreeHash
		}
		tree, err := s.applyLocked(base, map[string]treeOp{full: {content: []byte{}}})
		if err != nil {
			return planned{}, err
		}
		msg := "Initialize the control plane directory"
		if s.root != "" {
			msg = fmt.Sprintf("Initialize %s for the control plane", s.root)
		}
		return planned{tree: tree, author: s.cfg.Committer, message: msg + "\n"}, nil
	}

	out, err := s.commitLoop(ctx, plan)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("bootstrap %s: %w", s.cfg.Repository.Branch, err)
	}
	return BootstrapResult{Revision: out.commit.String(), Created: !out.unchanged}, nil
}
