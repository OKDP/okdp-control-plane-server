package flux

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// RequestSync sets a new value on the reconcile request annotation of the
// GitRepository. The Kustomization is never annotated: source-controller
// notifies kustomize-controller once the new artifact is stored, and waking
// both at once makes the Kustomization apply the previous artifact.
//
// Flux fetches the head of the branch, so revision is not sent.
//
// One patch runs at a time. Calls that arrive while a patch is in flight
// wait for it to end, then share a single patch that starts after all of
// them arrived. RequestSync returns once a patch started after the call has
// succeeded, or with the error of that patch, or when ctx ends.
func (a *Adapter) RequestSync(ctx context.Context, _ string) error {
	a.mu.Lock()
	a.requested++
	seq := a.requested
	for {
		switch {
		case a.done >= seq:
			a.mu.Unlock()
			return nil
		case a.failedUpTo >= seq:
			err := a.failErr
			a.mu.Unlock()
			return err
		case !a.inFlight:
			return a.patchLocked(ctx)
		}
		wait := a.patched
		a.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
		a.mu.Lock()
	}
}

// patchLocked sends one patch covering every call counted so far. It is
// entered with a.mu held and returns with it released.
func (a *Adapter) patchLocked(ctx context.Context) error {
	a.inFlight = true
	covers := a.requested
	value := a.nextRequestedAtLocked()
	a.mu.Unlock()

	err := a.patchRequestedAt(ctx, value)

	a.mu.Lock()
	a.inFlight = false
	switch {
	case err == nil:
		a.done = max(a.done, covers)
	case ctx.Err() == nil:
		// A failure of the patch itself is shared with the calls it covered.
		// A failure caused by this caller's ctx is not: they patch again.
		a.failedUpTo = max(a.failedUpTo, covers)
		a.failErr = err
	}
	close(a.patched)
	a.patched = make(chan struct{})
	a.mu.Unlock()
	return err
}

// nextRequestedAtLocked returns the clock as RFC 3339 with nanoseconds,
// moved forward by one nanosecond when it did not advance since the last
// value, so that every patch carries a new value.
func (a *Adapter) nextRequestedAtLocked() string {
	t := a.cfg.Now().UTC()
	if !t.After(a.lastRequestedAt) {
		t = a.lastRequestedAt.Add(time.Nanosecond)
	}
	a.lastRequestedAt = t
	return t.Format(time.RFC3339Nano)
}

func (a *Adapter) patchRequestedAt(ctx context.Context, value string) error {
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{ReconcileRequestAnnotation: value},
		},
	})
	if err != nil {
		return fmt.Errorf("flux: build the reconcile request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.PatchTimeout)
	defer cancel()
	if _, err := a.repos().Patch(ctx, a.cfg.GitRepositoryName, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("flux: request a reconcile of %s: %w", a.repoRef(), err)
	}
	return nil
}
