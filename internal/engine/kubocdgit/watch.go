package kubocdgit

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

const watchReopenBackoff = time.Second

// WatchInstances streams the changes of the merged view of a project: it
// recomputes the instances after a local write, a Release event, a new sync
// state, and at PollInterval, and sends what changed. The first instances
// come as ADDED.
func (a *Adapter) WatchInstances(ctx context.Context, project string) (<-chan engine.InstanceEvent, error) {
	wake, unsubscribe := a.subscribe()
	events := make(chan engine.InstanceEvent)
	go func() {
		defer close(events)
		defer unsubscribe()

		reopenTimer := time.NewTimer(watchReopenBackoff)
		reopenTimer.Stop()
		defer reopenTimer.Stop()
		var reopenC <-chan time.Time
		scheduleReopen := func() {
			reopenTimer.Reset(watchReopenBackoff)
			reopenC = reopenTimer.C
		}

		releases := a.watchReleases(ctx, project)
		if releases == nil {
			scheduleReopen()
		}
		defer func() {
			if releases != nil {
				releases.Stop()
			}
		}()
		ticker := time.NewTicker(a.pollInterval)
		defer ticker.Stop()

		known := map[engine.InstanceRef]engine.InstanceView{}
		for {
			if !a.sendChanges(ctx, project, known, events) {
				return
			}
			var releaseEvents <-chan watch.Event
			if releases != nil {
				releaseEvents = releases.ResultChan()
			}
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-ticker.C:
				if releases == nil {
					releases = a.watchReleases(ctx, project)
					if releases == nil {
						scheduleReopen()
					}
				}
			case <-reopenC:
				reopenC = nil
				if releases == nil {
					releases = a.watchReleases(ctx, project)
					if releases == nil {
						scheduleReopen()
					}
				}
			case _, ok := <-releaseEvents:
				if !ok || drainWatchEvents(releaseEvents) {
					releases.Stop()
					releases = nil
					scheduleReopen()
				}
			}
		}
	}()
	return events, nil
}

// drainWatchEvents consumes any burst of already-queued watch events so a
// wave of cluster updates triggers a single recomputation. It returns true if
// the watch channel closed while draining.
func drainWatchEvents(ch <-chan watch.Event) bool {
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return true
			}
		default:
			return false
		}
	}
}

// watchReleases opens a watch on every Release of the namespace, nil when it
// cannot: the poll and the other wake-ups still drive the stream.
func (a *Adapter) watchReleases(ctx context.Context, namespace string) watch.Interface {
	w, err := a.cluster.Resource(crd.GetReleaseGVR()).Namespace(namespace).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		logrus.WithError(err).WithField("project", namespace).Debug("Cannot watch the Releases, polling")
		return nil
	}
	return w
}

// sendChanges sends what changed since known and updates it. It returns
// false when ctx ended.
func (a *Adapter) sendChanges(ctx context.Context, project string, known map[engine.InstanceRef]engine.InstanceView, events chan<- engine.InstanceEvent) bool {
	views, listErr, err := a.observe(ctx, project, false)
	if err != nil {
		logrus.WithError(err).WithField("project", project).Debug("Cannot observe the instances for a watch")
		return ctx.Err() == nil
	}
	send := func(event engine.InstanceEvent) bool {
		select {
		case events <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}
	current := make(map[engine.InstanceRef]bool, len(views))
	for _, v := range views {
		current[v.Ref] = true
		prev, ok := known[v.Ref]
		switch {
		case !ok:
			if !send(engine.InstanceEvent{Type: engine.EventAdded, Instance: v}) {
				return false
			}
		case !reflect.DeepEqual(prev, v):
			if !send(engine.InstanceEvent{Type: engine.EventModified, Instance: v}) {
				return false
			}
		}
		known[v.Ref] = v
	}
	for ref, v := range known {
		if current[ref] {
			continue
		}
		if listErr != nil {
			unknown := v
			unknown.Status.Phase = engine.PhaseUnknown
			unknown.Status.Message = fmt.Sprintf("cannot read the Release: %v", listErr)
			if !reflect.DeepEqual(v, unknown) {
				if !send(engine.InstanceEvent{Type: engine.EventModified, Instance: unknown}) {
					return false
				}
				known[ref] = unknown
			}
			continue
		}
		if !send(engine.InstanceEvent{Type: engine.EventDeleted, Instance: v}) {
			return false
		}
		delete(known, ref)
	}
	return true
}
