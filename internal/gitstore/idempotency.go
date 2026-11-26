package gitstore

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// keyRecord is the commit that first used an idempotency key.
type keyRecord struct {
	commit plumbing.Hash
	digest string
}

// keyIndex maps the idempotency keys found in the history to their commit.
// It grows with each new head, from the commits not indexed yet. It is used
// under writeMu only.
type keyIndex struct {
	seen map[plumbing.Hash]bool
	keys map[string]keyRecord
}

func newKeyIndex() keyIndex {
	return keyIndex{seen: map[plumbing.Hash]bool{}, keys: map[string]keyRecord{}}
}

// lookupKey finds the commit reachable from head that carries key. The
// caller holds writeMu.
func (s *Store) lookupKey(head plumbing.Hash, key string) (keyRecord, bool, error) {
	if head.IsZero() {
		return keyRecord{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.indexLocked(head); err != nil {
		return keyRecord{}, false, err
	}
	rec, ok := s.keys.keys[key]
	if !ok {
		return keyRecord{}, false, nil
	}
	reachable, err := s.isAncestorLocked(rec.commit, head)
	if err != nil || reachable {
		return rec, reachable, err
	}
	// The branch was rewritten: rebuild from what head reaches.
	s.keys = newKeyIndex()
	if err := s.indexLocked(head); err != nil {
		return keyRecord{}, false, err
	}
	rec, ok = s.keys.keys[key]
	return rec, ok, nil
}

// indexLocked indexes the commits reachable from head that are not indexed
// yet. When a key appears in several commits, the oldest wins. The caller
// holds mu and writeMu.
func (s *Store) indexLocked(head plumbing.Hash) error {
	queue := []plumbing.Hash{head}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if s.keys.seen[h] {
			continue
		}
		c, err := object.GetCommit(s.repo.Storer, h)
		if err != nil {
			return err
		}
		s.keys.seen[h] = true
		if key, digest, ok := parseTrailers(c.Message); ok {
			prev, exists := s.keys.keys[key]
			if !exists || s.olderLocked(h, prev.commit) {
				s.keys.keys[key] = keyRecord{commit: h, digest: digest}
			}
		}
		queue = append(queue, c.ParentHashes...)
	}
	return nil
}

// olderLocked reports whether a is an ancestor of b, else whether it was
// committed first.
func (s *Store) olderLocked(a, b plumbing.Hash) bool {
	if ok, err := s.isAncestorLocked(a, b); err == nil && ok {
		return true
	}
	ca, errA := object.GetCommit(s.repo.Storer, a)
	cb, errB := object.GetCommit(s.repo.Storer, b)
	if errA != nil || errB != nil {
		return false
	}
	return ca.Committer.When.Before(cb.Committer.When)
}

// parseTrailers reads the idempotency trailers of the last paragraph of a
// commit message.
func parseTrailers(message string) (key, digest string, ok bool) {
	message = strings.TrimSpace(message)
	if i := strings.LastIndex(message, "\n\n"); i >= 0 {
		message = message[i+2:]
	} else {
		return "", "", false
	}
	for _, line := range strings.Split(message, "\n") {
		name, value, found := strings.Cut(line, ": ")
		if !found {
			continue
		}
		switch name {
		case trailerKey:
			key = value
		case trailerDigest:
			digest = value
		}
	}
	return key, digest, key != "" && digest != ""
}

type digest struct{ h hash.Hash }

func newDigest() *digest { return &digest{h: sha256.New()} }

// line writes fields separated by NUL and terminated by a newline. None of
// them can hold either: paths and revisions are checked, the others are
// fixed words or hex.
func (d *digest) line(fields ...string) {
	d.h.Write([]byte(strings.Join(fields, "\x00") + "\n"))
}

func (d *digest) sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (d *digest) hex() string { return hex.EncodeToString(d.h.Sum(nil)) }
