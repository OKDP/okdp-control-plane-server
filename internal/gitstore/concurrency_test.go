package gitstore

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two replicas, two Stores on one remote, plan their write on the same head
// and push together: the barrier makes the race certain.
func raceTwo(t *testing.T, bare string, a, b WriteRequest) (results [2]*WriteResult, errs [2]error, replays int32) {
	t.Helper()
	bar := newBarrier(2)
	var wg sync.WaitGroup
	for i, req := range []WriteRequest{a, b} {
		s := newStore(t, bare)
		s.beforePush = bar.hook()
		// Two identical requests committed in the same second would make
		// the very same commit, which is not a race.
		offset := time.Duration(i) * time.Hour
		s.now = func() time.Time { return time.Now().Add(offset) }
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.Write(testCtx(t), req)
		}()
	}
	wg.Wait()
	return results, errs, bar.replays.Load()
}

func TestConcurrentWritesOnTheSameFileGiveOneSuccessAndOneConflict(t *testing.T) {
	bare, s := bootstrapped(t)
	path, err := ReleaseFile("team-a", "spark")
	require.NoError(t, err)
	base := write(t, s, path, "v: 0\n")
	head := headOf(t, bare)

	results, errs, _ := raceTwo(t, bare,
		WriteRequest{Changes: []Change{{Path: path, Content: []byte("v: alice\n"), ExpectedRevision: base.Revision}}},
		WriteRequest{Changes: []Change{{Path: path, Content: []byte("v: bob\n"), ExpectedRevision: base.Revision}}},
	)

	var winner int
	switch {
	case errs[0] == nil && errs[1] != nil:
		winner = 0
	case errs[1] == nil && errs[0] != nil:
		winner = 1
	default:
		t.Fatalf("want one success and one conflict, got %v and %v", errs[0], errs[1])
	}
	loser := 1 - winner
	assert.ErrorIs(t, errs[loser], ErrConflict)
	var conflict *ConflictError
	require.ErrorAs(t, errs[loser], &conflict)
	assert.Equal(t, base.Revision, conflict.Expected)
	assert.Equal(t, results[winner].Revision, conflict.Actual)

	assert.Equal(t, []string{"v: alice", "v: bob"}[winner], showFile(t, bare, "clusters/dev/"+path), "the winner's content, not overwritten")
	assert.Equal(t, head, runGit(t, bare, "rev-parse", "main^"), "exactly one new commit")
	fsck(t, bare)
}

func TestConcurrentCreatesOfTheSameFileGiveOneSuccessAndOneExists(t *testing.T) {
	bare, _ := bootstrapped(t)
	path, err := NamespaceFile("team-a")
	require.NoError(t, err)

	_, errs, _ := raceTwo(t, bare,
		WriteRequest{Changes: []Change{{Path: path, Content: []byte("alice\n"), MustNotExist: true}}},
		WriteRequest{Changes: []Change{{Path: path, Content: []byte("bob\n"), MustNotExist: true}}},
	)
	if errs[0] == nil {
		assert.ErrorIs(t, errs[1], ErrAlreadyExists)
	} else {
		require.NoError(t, errs[1])
		assert.ErrorIs(t, errs[0], ErrAlreadyExists)
	}
	assert.Equal(t, "2", commitCount(t, bare))
}

func TestConcurrentWritesOnTwoProjectsBothSucceed(t *testing.T) {
	bare, s := bootstrapped(t)
	a, err := ReleaseFile("team-a", "spark")
	require.NoError(t, err)
	b, err := ReleaseFile("team-b", "trino")
	require.NoError(t, err)
	baseA := write(t, s, a, "a: 0\n")
	baseB := write(t, s, b, "b: 0\n")
	head := headOf(t, bare)

	results, errs, replays := raceTwo(t, bare,
		WriteRequest{Changes: []Change{{Path: a, Content: []byte("a: 1\n"), ExpectedRevision: baseA.Revision}}},
		WriteRequest{Changes: []Change{{Path: b, Content: []byte("b: 1\n"), ExpectedRevision: baseB.Revision}}},
	)
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.Equal(t, int32(1), replays, "the loser replayed its write on the winner's commit")

	assert.Equal(t, "a: 1", showFile(t, bare, "clusters/dev/"+a))
	assert.Equal(t, "b: 1", showFile(t, bare, "clusters/dev/"+b))
	assert.Equal(t, head, runGit(t, bare, "rev-parse", "main~2"), "two new commits, in a line")
	assert.Equal(t, results[0].Files[a], results[0].Revision)
	assert.Equal(t, results[1].Files[b], results[1].Revision)
	fsck(t, bare)
}

func TestManyWritersAndReadersOnOneRemote(t *testing.T) {
	bare, _ := bootstrapped(t)
	stores := []*Store{newStore(t, bare, withAttempts(50)), newStore(t, bare, withAttempts(50))}
	const writers, writesEach = 6, 4

	var wg sync.WaitGroup
	errs := make(chan error, writers*writesEach)
	for w := range writers {
		s := stores[w%len(stores)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range writesEach {
				path := fmt.Sprintf("team-%d/releases/r%d.yaml", w, i)
				_, err := s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: path, Content: []byte(path), MustNotExist: true}}})
				errs <- err
			}
		}()
	}
	// Readers share the storage of the writing stores.
	done := make(chan struct{})
	var readers sync.WaitGroup
	for _, s := range stores {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				snap, err := s.Latest(testCtx(t))
				if !assert.NoError(t, err) {
					return
				}
				entries, err := snap.ReadDir("")
				if !assert.NoError(t, err) {
					return
				}
				for _, e := range entries {
					if e.Kind == KindDir {
						_, err := snap.FileRevision(e.Name)
						assert.NoError(t, err)
					}
				}
				if snap.Revision() == "" {
					continue
				}
				at, err := s.At(testCtx(t), snap.Revision())
				if !assert.NoError(t, err) {
					return
				}
				_, err = at.ReadFile(".gitkeep")
				assert.NoError(t, err)
			}
		}()
	}
	wg.Wait()
	close(done)
	readers.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
	assert.Equal(t, fmt.Sprint(1+writers*writesEach), commitCount(t, bare))
	fsck(t, bare)
}
