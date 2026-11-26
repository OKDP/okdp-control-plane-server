package gitstore

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// treeOp writes content to a file, deletes it, or deletes a directory.
type treeOp struct {
	content    []byte
	delete     bool
	deleteTree bool
}

// applyLocked applies ops, keyed by path from the repository root, to the
// tree base (zero for none) and returns the new root tree. It never
// replaces a directory, a symlink or a submodule with a file, nor goes
// through one, and drops the directories a deletion empties. The caller
// holds mu for writing.
func (s *Store) applyLocked(base plumbing.Hash, ops map[string]treeOp) (plumbing.Hash, error) {
	h, empty, err := s.applyDirLocked(base, "", ops)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if empty {
		return s.storeTreeLocked(nil)
	}
	return h, nil
}

func (s *Store) applyDirLocked(base plumbing.Hash, prefix string, ops map[string]treeOp) (plumbing.Hash, bool, error) {
	entries := map[string]object.TreeEntry{}
	if !base.IsZero() {
		tree, err := object.GetTree(s.repo.Storer, base)
		if err != nil {
			return plumbing.ZeroHash, false, err
		}
		for _, e := range tree.Entries {
			entries[e.Name] = e
		}
	}

	direct := map[string]treeOp{}
	nested := map[string]map[string]treeOp{}
	for rel, op := range ops {
		name, rest, deeper := strings.Cut(rel, "/")
		if !deeper {
			direct[name] = op
			continue
		}
		if nested[name] == nil {
			nested[name] = map[string]treeOp{}
		}
		nested[name][rest] = op
	}

	for name, op := range direct {
		e, exists := entries[name]
		if op.deleteTree {
			if !exists || e.Mode != filemode.Dir {
				return plumbing.ZeroHash, false, fmt.Errorf("%w: %s is not a directory", ErrNotFound, prefix+name)
			}
			delete(entries, name)
			continue
		}
		if exists && kindOf(e.Mode) != KindFile {
			return plumbing.ZeroHash, false, fmt.Errorf("%w: %s is a %s, not a regular file", ErrInvalidPath, prefix+name, kindOf(e.Mode))
		}
		if op.delete {
			if !exists {
				return plumbing.ZeroHash, false, fmt.Errorf("%s: %w", prefix+name, ErrNotFound)
			}
			delete(entries, name)
			continue
		}
		blob, err := s.storeBlobLocked(op.content)
		if err != nil {
			return plumbing.ZeroHash, false, err
		}
		mode := filemode.Regular
		if exists && e.Mode == filemode.Executable {
			mode = filemode.Executable
		}
		entries[name] = object.TreeEntry{Name: name, Mode: mode, Hash: blob}
	}

	for name, sub := range nested {
		subBase := plumbing.ZeroHash
		if e, exists := entries[name]; exists {
			if e.Mode != filemode.Dir {
				return plumbing.ZeroHash, false, fmt.Errorf("%w: %s is a %s, not a directory", ErrInvalidPath, prefix+name, kindOf(e.Mode))
			}
			subBase = e.Hash
		}
		h, empty, err := s.applyDirLocked(subBase, prefix+name+"/", sub)
		if err != nil {
			return plumbing.ZeroHash, false, err
		}
		if empty {
			delete(entries, name)
		} else {
			entries[name] = object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: h}
		}
	}

	if len(entries) == 0 {
		return plumbing.ZeroHash, true, nil
	}
	list := make([]object.TreeEntry, 0, len(entries))
	for _, e := range entries {
		list = append(list, e)
	}
	h, err := s.storeTreeLocked(list)
	return h, false, err
}

// storeTreeLocked writes a tree with its entries in git order: by name, a
// directory compared as if its name ended with a slash.
func (s *Store) storeTreeLocked(entries []object.TreeEntry) (plumbing.Hash, error) {
	sortKey := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(entries, func(i, j int) bool { return sortKey(entries[i]) < sortKey(entries[j]) })

	obj := s.repo.Storer.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return s.repo.Storer.SetEncodedObject(obj)
}

func (s *Store) storeBlobLocked(content []byte) (plumbing.Hash, error) {
	obj := s.repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(content); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return s.repo.Storer.SetEncodedObject(obj)
}

func (s *Store) storeCommitLocked(c *object.Commit) (plumbing.Hash, error) {
	obj := s.repo.Storer.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return s.repo.Storer.SetEncodedObject(obj)
}
