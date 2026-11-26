package kubocd

import (
	"context"
	"fmt"
	"strconv"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

// ReleaseView describes a Release as an observed instance.
func ReleaseView(r *crd.Release) engine.InstanceView {
	serviceName := r.Labels[crd.LabelService]
	instanceName := r.Labels[crd.LabelInstanceName]
	if instanceName == "" {
		instanceName = serviceName
	}

	// The controller lists the connections it resolved and, separately, every
	// candidate it watches, resolved or not. Both are kept as they come.
	bindings := make([]engine.ConnectionBinding, 0, len(r.Status.EffectiveInputConnections)+len(r.Status.WatchedInputConnections))
	for _, ref := range r.Status.EffectiveInputConnections {
		bindings = append(bindings, engine.ConnectionBinding{Kind: ref.Kind, Name: ref.Name, Namespace: ref.Namespace, Bound: true})
	}
	for _, ref := range r.Status.WatchedInputConnections {
		bindings = append(bindings, engine.ConnectionBinding{Kind: ref.Kind, Name: ref.Name, Namespace: ref.Namespace})
	}

	applied := ""
	if r.Status.ObservedGeneration > 0 {
		applied = strconv.FormatInt(r.Status.ObservedGeneration, 10)
	}

	return engine.InstanceView{
		Ref:             engine.InstanceRef{Project: r.Namespace, Name: instanceName},
		EngineName:      r.Name,
		Service:         serviceName,
		Version:         r.Spec.Package.Tag,
		Parameters:      r.Spec.Parameters,
		TargetNamespace: r.Spec.TargetNamespace,
		Roles:           r.Status.Roles,
		Bindings:        bindings,
		CreatedAt:       r.CreationTimestamp.Time,
		Status: engine.InstanceStatus{
			Phase:           Phase(r.Status.Phase),
			EnginePhase:     r.Status.Phase,
			Deleting:        r.DeletionTimestamp != nil,
			Message:         r.Status.Message,
			DesiredRevision: revision(r),
			AppliedRevision: applied,
		},
	}
}

func (a *Adapter) ObserveInstance(ctx context.Context, ref engine.InstanceRef) (engine.InstanceView, error) {
	release, err := a.releases.Get(ctx, ref.Project, ReleaseName(ref))
	if err != nil {
		return engine.InstanceView{}, classify(err)
	}
	views := []engine.InstanceView{ReleaseView(release)}
	a.withConnectionStates(ctx, release.Namespace, views, nil)
	return views[0], nil
}

func (a *Adapter) ObserveInstances(ctx context.Context, project string) ([]engine.InstanceView, error) {
	releases, err := a.releases.List(ctx, project, project)
	if err != nil {
		return nil, err
	}
	var views []engine.InstanceView
	for i := range releases {
		views = append(views, ReleaseView(&releases[i]))
	}
	a.withConnectionStates(ctx, project, views, releases)
	return views, nil
}

func (a *Adapter) EngineName(ref engine.InstanceRef) string {
	return ReleaseName(ref)
}

// Workloads names the HelmReleases the Release owns: their names are the
// app.kubernetes.io/instance labels of what they deploy.
func (a *Adapter) Workloads(ctx context.Context, namespace, name string) (engine.Workloads, error) {
	release, err := a.releases.Get(ctx, namespace, name)
	if err != nil {
		return engine.Workloads{}, fmt.Errorf("failed to get release: %w", classify(err))
	}

	helmReleases, err := a.cluster.Resource(crd.GetHelmReleaseGVR()).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return engine.Workloads{}, fmt.Errorf("failed to list helm releases: %w", err)
	}

	var names []string
	for _, hr := range helmReleases.Items {
		for _, owner := range hr.GetOwnerReferences() {
			if owner.UID == release.UID {
				names = append(names, hr.GetName())
				break
			}
		}
	}
	if len(names) == 0 {
		return engine.Workloads{}, fmt.Errorf("no helm release owned by %s", name)
	}
	return engine.Workloads{Names: names}, nil
}

// WatchInstances relays the Release watch of a project. Events whose object is
// not a Release (watch errors) are dropped. The channel closes when ctx ends
// or the Kubernetes watch stops.
func (a *Adapter) WatchInstances(ctx context.Context, project string) (<-chan engine.InstanceEvent, error) {
	watcher, err := a.releases.Watch(ctx, project, project)
	if err != nil {
		return nil, err
	}

	events := make(chan engine.InstanceEvent)
	go func() {
		defer close(events)
		defer watcher.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.ResultChan():
				if !ok {
					return
				}
				u, ok := event.Object.(*unstructured.Unstructured)
				if !ok {
					continue
				}
				var release crd.Release
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &release); err != nil {
					logrus.WithError(err).Warn("Skipping a Release the watch could not decode")
					continue
				}
				select {
				case events <- engine.InstanceEvent{Type: engine.EventType(event.Type), Instance: ReleaseView(&release)}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return events, nil
}
