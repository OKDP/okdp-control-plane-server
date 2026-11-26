package flux

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
)

// ObserveSync reads both Flux objects and reports their state. A missing
// object gives an Unknown state and no error. Any other read failure gives
// an Unknown state and the error.
func (a *Adapter) ObserveSync(ctx context.Context) (gitops.SyncState, error) {
	repo, err := getOptional(ctx, a.repos(), a.cfg.GitRepositoryName)
	if err != nil {
		return a.unknown(fmt.Errorf("flux: read %s: %w", a.repoRef(), err))
	}
	ks, err := getOptional(ctx, a.kustomizations(), a.cfg.KustomizationName)
	if err != nil {
		return a.unknown(fmt.Errorf("flux: read %s: %w", a.ksRef(), err))
	}
	return a.computeState(repo, ks, a.cfg.Now()), nil
}

func (a *Adapter) unknown(err error) (gitops.SyncState, error) {
	return gitops.SyncState{Phase: gitops.SyncUnknown, Message: err.Error(), ObservedAt: a.cfg.Now()}, err
}

func getOptional(ctx context.Context, ri dynamic.ResourceInterface, name string) (*unstructured.Unstructured, error) {
	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return obj, err
}

// WatchSync lists both Flux objects, watches them from that point, and
// sends the state on the returned channel: the current one first, then
// each change. A state equal to the previous one, ObservedAt aside, is not
// sent again. The current state is always delivered first. After it, a slow
// reader gets only the state current when it reads, and never blocks the
// watch.
//
// The channel is closed when ctx ends, or when the API server ends either
// watch or reports a watch error. The caller then calls WatchSync again.
func (a *Adapter) WatchSync(ctx context.Context) (<-chan gitops.SyncState, error) {
	repoW, repo, err := a.listAndWatch(ctx, a.repos(), a.cfg.GitRepositoryName)
	if err != nil {
		return nil, fmt.Errorf("flux: watch %s: %w", a.repoRef(), err)
	}
	ksW, ks, err := a.listAndWatch(ctx, a.kustomizations(), a.cfg.KustomizationName)
	if err != nil {
		repoW.Stop()
		return nil, fmt.Errorf("flux: watch %s: %w", a.ksRef(), err)
	}

	out := make(chan gitops.SyncState)
	last := a.computeState(repo, ks, a.cfg.Now())
	go func() {
		defer close(out)
		defer repoW.Stop()
		defer ksW.Stop()
		// out is unbuffered. initial waits until the reader takes it, and
		// latest holds the newest state after it, so a reader gets the
		// initial state, then only the state current when it reads.
		first := last
		initial, latest := &first, (*gitops.SyncState)(nil)
		for {
			var send chan<- gitops.SyncState
			var next gitops.SyncState
			switch {
			case initial != nil:
				send, next = out, *initial
			case latest != nil:
				send, next = out, *latest
			}
			var ok bool
			select {
			case <-ctx.Done():
				return
			case send <- next:
				if initial != nil {
					initial = nil
				} else {
					latest = nil
				}
				continue
			case ev, open := <-repoW.ResultChan():
				repo, ok = a.apply(ev, open, repo, a.cfg.GitRepositoryName)
			case ev, open := <-ksW.ResultChan():
				ks, ok = a.apply(ev, open, ks, a.cfg.KustomizationName)
			}
			if !ok {
				return
			}
			st := a.computeState(repo, ks, a.cfg.Now())
			if sameState(st, last) {
				continue
			}
			last = st
			latest = &st
		}
	}()
	return out, nil
}

const watchListTimeout = 30 * time.Second

// listAndWatch returns the object named name, nil when it does not exist,
// and a watch started at the resource version of that read.
func (a *Adapter) listAndWatch(ctx context.Context, ri dynamic.ResourceInterface, name string) (watch.Interface, *unstructured.Unstructured, error) {
	selector := fields.OneTermEqualSelector("metadata.name", name).String()
	listCtx, cancel := context.WithTimeout(ctx, watchListTimeout)
	list, err := ri.List(listCtx, metav1.ListOptions{FieldSelector: selector})
	cancel()
	if err != nil {
		return nil, nil, err
	}
	var obj *unstructured.Unstructured
	for i := range list.Items {
		if list.Items[i].GetName() == name {
			obj = &list.Items[i]
		}
	}
	w, err := ri.Watch(ctx, metav1.ListOptions{
		FieldSelector:   selector,
		ResourceVersion: list.GetResourceVersion(),
	})
	if err != nil {
		return nil, nil, err
	}
	return w, obj, nil
}

// apply folds a watch event into the current object. It returns false when
// the stream must end: the watch channel closed or reported an error.
func (a *Adapter) apply(ev watch.Event, open bool, cur *unstructured.Unstructured, name string) (*unstructured.Unstructured, bool) {
	if !open || ev.Type == watch.Error {
		return cur, false
	}
	obj, isObj := ev.Object.(*unstructured.Unstructured)
	if !isObj || obj.GetName() != name {
		return cur, true
	}
	switch ev.Type {
	case watch.Added, watch.Modified:
		return obj, true
	case watch.Deleted:
		return nil, true
	}
	return cur, true
}

func sameState(x, y gitops.SyncState) bool {
	x.ObservedAt = y.ObservedAt
	return x == y
}
