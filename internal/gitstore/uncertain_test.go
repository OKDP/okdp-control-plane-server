package gitstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errReset = errors.New("read tcp: connection reset by peer")

func realPush(s *Store) func(context.Context, *git.PushOptions) error {
	return func(ctx context.Context, opts *git.PushOptions) error { return s.repo.PushContext(ctx, opts) }
}

func oneFile(p, content string) WriteRequest {
	return WriteRequest{Changes: []Change{{Path: p, Content: []byte(content)}}}
}

func TestAPushThatLandedDespiteAnErrorIsASuccess(t *testing.T) {
	bare, s := bootstrapped(t)
	s.pushFn = func(ctx context.Context, opts *git.PushOptions) error {
		if err := realPush(s)(ctx, opts); err != nil {
			return err
		}
		return errReset
	}
	res, err := s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
	require.NoError(t, err)
	assert.Equal(t, headOf(t, bare), res.Revision)
	assert.Equal(t, "2", commitCount(t, bare))
}

func TestAnAmbiguousPushThatNeverLandsIsUncertain(t *testing.T) {
	bare, s := bootstrapped(t, withAttempts(3))
	head := headOf(t, bare)
	s.pushFn = func(context.Context, *git.PushOptions) error { return errReset }

	_, err := s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
	assert.ErrorIs(t, err, ErrUncertain)
	assertOnlyUncertain(t, err)
	assert.Equal(t, head, headOf(t, bare))
}

func TestAnAmbiguousPushWithTheRemoteGoneIsUncertain(t *testing.T) {
	bare, s := bootstrapped(t)
	moved := bare + ".away"
	s.pushFn = func(context.Context, *git.PushOptions) error {
		require.NoError(t, os.Rename(bare, moved))
		return errReset
	}
	_, err := s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
	assert.ErrorIs(t, err, ErrUncertain)
	assertOnlyUncertain(t, err)
	require.NoError(t, os.Rename(moved, bare))
}

func TestCancellingAfterAPushThatLandedIsASuccess(t *testing.T) {
	bare, s := bootstrapped(t)
	ctx, cancel := context.WithCancel(testCtx(t))
	s.pushFn = func(pctx context.Context, opts *git.PushOptions) error {
		if err := realPush(s)(pctx, opts); err != nil {
			return err
		}
		cancel()
		return context.Canceled
	}
	res, err := s.Write(ctx, oneFile("a.yaml", "a\n"))
	require.NoError(t, err, "the settling fetch runs on a fresh context")
	assert.Equal(t, headOf(t, bare), res.Revision)
}

func TestCancellingAfterAnAmbiguousPushIsUncertain(t *testing.T) {
	bare, s := bootstrapped(t)
	head := headOf(t, bare)
	ctx, cancel := context.WithCancel(testCtx(t))
	s.pushFn = func(context.Context, *git.PushOptions) error {
		cancel()
		return context.Canceled
	}
	_, err := s.Write(ctx, oneFile("a.yaml", "a\n"))
	assert.ErrorIs(t, err, ErrUncertain)
	assertOnlyUncertain(t, err)
	assert.Equal(t, head, headOf(t, bare))
}

func TestAnAmbiguousPushOvertakenByAnotherCommitIsReplayed(t *testing.T) {
	bare, s := bootstrapped(t)
	var calls atomic.Int32
	s.pushFn = func(ctx context.Context, opts *git.PushOptions) error {
		if calls.Add(1) == 1 {
			pushDirect(t, bare, "main", map[string][]byte{"clusters/dev/b.yaml": []byte("b\n")}, "someone else")
			return errReset
		}
		return realPush(s)(ctx, opts)
	}
	res, err := s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
	require.NoError(t, err, "the branch moved past the lost push: it can never land")
	assert.Equal(t, int32(2), calls.Load())
	assert.Equal(t, headOf(t, bare), res.Revision)
	assert.Equal(t, "a", showFile(t, bare, "clusters/dev/a.yaml"))
	assert.Equal(t, "b", showFile(t, bare, "clusters/dev/b.yaml"))
}

func TestAnAmbiguousPushThatLandsLateIsFound(t *testing.T) {
	bare, s := bootstrapped(t)
	s.now = ticking()
	var first, second plumbing.Hash
	var calls atomic.Int32
	s.pushFn = func(ctx context.Context, opts *git.PushOptions) error {
		ref, err := s.repo.Storer.Reference(s.branch)
		require.NoError(t, err)
		if calls.Add(1) == 1 {
			first = ref.Hash()
			return errReset
		}
		second = ref.Hash()
		// The first push reaches the remote while the second is on its way.
		delayed := plumbing.ReferenceName("refs/heads/delayed")
		require.NoError(t, s.repo.Storer.SetReference(plumbing.NewHashReference(delayed, first)))
		require.NoError(t, s.repo.PushContext(ctx, &git.PushOptions{
			RemoteName: remoteName,
			RefSpecs:   []config.RefSpec{config.RefSpec(delayed + ":" + s.branch)},
		}))
		return realPush(s)(ctx, opts)
	}
	res, err := s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
	require.NoError(t, err)
	assert.NotEqual(t, first, second, "two distinct attempts")
	assert.Equal(t, first.String(), res.Revision)
	assert.Equal(t, first.String(), headOf(t, bare))
	assert.Equal(t, "2", commitCount(t, bare), "the second attempt did not land on top")
}

func TestAHookRejectionIsRejected(t *testing.T) {
	bare, s := bootstrapped(t)
	head := headOf(t, bare)
	installHook(t, bare)
	var calls atomic.Int32
	s.pushFn = func(ctx context.Context, opts *git.PushOptions) error {
		calls.Add(1)
		return realPush(s)(ctx, opts)
	}

	_, err := s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
	assert.ErrorIs(t, err, ErrRejected)
	assert.NotErrorIs(t, err, ErrUncertain)
	assert.Equal(t, int32(1), calls.Load(), "a refusal is not retried")
	assert.Equal(t, head, headOf(t, bare))
}

func TestABranchThatNeverStopsMovingIsBusy(t *testing.T) {
	bare, s := bootstrapped(t, withAttempts(3))
	var n atomic.Int32
	s.pushFn = func(ctx context.Context, opts *git.PushOptions) error {
		i := n.Add(1)
		pushDirect(t, bare, "main", map[string][]byte{"clusters/dev/other.yaml": []byte{byte('0' + i)}}, "someone else")
		return realPush(s)(ctx, opts)
	}
	_, err := s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
	assert.ErrorIs(t, err, ErrBusy)
	assert.NotErrorIs(t, err, ErrUncertain, "every push was refused before it was sent")
}

func TestAnUnreachableRemoteIsUnavailable(t *testing.T) {
	s := newStore(t, filepath.Join(t.TempDir(), "missing.git"))
	_, err := s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.NotErrorIs(t, err, ErrUncertain)
	_, err = s.Latest(testCtx(t))
	assert.ErrorIs(t, err, ErrUnavailable)
}

// assertOnlyUncertain checks that an uncertain error matches none of the
// sentinels that promise nothing was written.
func assertOnlyUncertain(t *testing.T, err error) {
	t.Helper()
	for _, certain := range []error{ErrRejected, ErrBusy, ErrUnavailable, ErrConflict, ErrAlreadyExists, ErrNotFound} {
		assert.NotErrorIs(t, err, certain)
	}
}

func TestClassifyPush(t *testing.T) {
	for msg, want := range map[string]pushKind{
		"remote ref refs/heads/main required to be abc but is def":                 pushLost,
		"non-fast-forward update: refs/heads/main":                                 pushLost,
		"command error on refs/heads/main: failed to update ref":                   pushLost,
		"command error on refs/heads/main: failed to lock":                         pushLost,
		"command error on refs/heads/main: incorrect old value provided":           pushLost,
		"command error on refs/heads/main: pre-receive hook declined":              pushRefused,
		"command error on refs/heads/main: hook says ref required to be kept":      pushRejected,
		"command error on refs/heads/main: reference already exists":               pushLost,
		"command error on refs/heads/main: branch is protected":                    pushRefused,
		"command error on refs/heads/protected-main: incorrect old value provided": pushLost,
		"command error on refs/heads/deny-list: failed to lock":                    pushLost,
		"unpack error: index-pack failed":                                          pushRefused,
		"read tcp: connection reset by peer":                                       pushAmbiguous,
		"context deadline exceeded":                                                pushAmbiguous,
	} {
		assert.Equal(t, want, classifyPush(errors.New(msg)), msg)
	}
	for _, err := range []error{transport.ErrAuthorizationFailed, transport.ErrAuthenticationRequired, transport.ErrRepositoryNotFound} {
		assert.Equal(t, pushRefused, classifyPush(fmt.Errorf("push: %w", err)), err.Error())
	}
}

// ticking returns a clock one minute later at each call, so that two
// attempts never make the same commit.
func ticking() func() time.Time {
	var n atomic.Int64
	start := time.Now()
	return func() time.Time { return start.Add(time.Duration(n.Add(1)) * time.Minute) }
}

func installHook(t *testing.T, bare string) func() {
	hook := filepath.Join(bare, "hooks", "pre-receive")
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\necho 'protected branch' >&2\nexit 1\n"), 0o755))
	return func() { require.NoError(t, os.Remove(hook)) }
}

func TestARejectionAfterAnAmbiguousPushIsUncertain(t *testing.T) {
	bare, s := bootstrapped(t)
	s.now = ticking()
	head := headOf(t, bare)
	var calls atomic.Int32
	s.pushFn = func(ctx context.Context, opts *git.PushOptions) error {
		if calls.Add(1) == 1 {
			return errReset
		}
		installHook(t, bare)
		return realPush(s)(ctx, opts)
	}
	_, err := s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
	assert.ErrorIs(t, err, ErrUncertain, "the first push may still land")
	assertOnlyUncertain(t, err)
	assert.Equal(t, head, headOf(t, bare))
}

func TestARejectionStaysARejection(t *testing.T) {
	cases := map[string]func(t *testing.T, bare string){
		"the branch moves": func(t *testing.T, bare string) {
			remove := installHook(t, bare)
			remove()
			pushDirect(t, bare, "main", map[string][]byte{"clusters/dev/b.yaml": []byte("b\n")}, "someone else")
			installHook(t, bare)
		},
		"the remote goes away": func(t *testing.T, bare string) {
			require.NoError(t, os.Rename(bare, bare+".away"))
			t.Cleanup(func() { _ = os.Rename(bare+".away", bare) })
		},
	}
	for name, after := range cases {
		t.Run(name, func(t *testing.T) {
			bare, s := bootstrapped(t)
			installHook(t, bare)
			var calls atomic.Int32
			s.pushFn = func(ctx context.Context, opts *git.PushOptions) error {
				calls.Add(1)
				err := realPush(s)(ctx, opts)
				after(t, bare)
				return err
			}
			_, err := s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
			assert.ErrorIs(t, err, ErrRejected)
			assert.NotErrorIs(t, err, ErrUncertain)
			assert.Equal(t, int32(1), calls.Load())
		})
	}
}

func TestARewindKeepsALostPushUncertain(t *testing.T) {
	bare, s := bootstrapped(t, withAttempts(2))
	s.now = ticking()
	h := headOf(t, bare)
	var calls atomic.Int32
	s.pushFn = func(context.Context, *git.PushOptions) error {
		switch calls.Add(1) {
		case 1:
			// The branch moves past the first push: it looks lost.
			pushDirect(t, bare, "main", map[string][]byte{"clusters/dev/b.yaml": []byte("b\n")}, "J")
		case 2:
			// Then someone force-resets it to a sibling of J.
			k := runGit(t, bare, "commit-tree", h+"^{tree}", "-p", h, "-m", "K")
			runGit(t, bare, "update-ref", "refs/heads/main", k)
		}
		return errReset
	}
	_, err := s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
	assert.ErrorIs(t, err, ErrUncertain, "after a rewind, a moved head proves nothing")
	assertOnlyUncertain(t, err)
}
