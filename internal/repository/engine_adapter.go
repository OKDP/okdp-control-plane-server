package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// Engine phases, the vocabulary of the console (models.ServiceInstance.Status).
const (
	PhaseReady      = "Ready"
	PhaseInstalling = "Installing"
	PhaseUpdating   = "Updating"
	PhaseError      = "Error"
	// PhasePending is a declaration committed to Git that the engine has not
	// picked up yet: no HelmRelease or Application exists for it.
	PhasePending = "Pending"
)

// EngineStatus is what the GitOps engine reports for one release.
type EngineStatus struct {
	// Found is false while the engine has no object for the release.
	Found bool
	// Phase is one of the Phase* constants.
	Phase   string
	Message string
	// Revision is the chart version the engine is rendering, "" when unknown.
	Revision string
	// CreatedAt is when the engine object appeared.
	CreatedAt time.Time
}

// EngineAdapter reads the render/sync status of project releases from the
// objects of the configured GitOps engine. Read-only: the server never
// creates them, the engine does from Git.
type EngineAdapter interface {
	// Name is "flux" or "argocd".
	Name() string
	// List returns the status of every release targeting project, by release name.
	List(ctx context.Context, project string) (map[string]EngineStatus, error)
	// Get returns the status of one release; Found is false when there is none.
	Get(ctx context.Context, project, release string) (EngineStatus, error)
	// Watch streams changes of the engine objects; ReleaseOf maps an event
	// object to the project and release it belongs to.
	Watch(ctx context.Context) (watch.Interface, error)
	ReleaseOf(obj *unstructured.Unstructured) (project, release string)
}

// Labels every HelmRelease, OCIRepository and Application of the layout
// carries (render-flux.sh and the ApplicationSets set them).
const (
	LabelEngineProject  = "okdp.io/project"
	LabelEngineInstance = "okdp.io/instance"
)

// releaseOf reads the project and release of an engine object from its
// labels, falling back to the fields that carry the same information.
func releaseOf(obj *unstructured.Unstructured, projectPath []string, releasePath []string) (string, string) {
	labels := obj.GetLabels()
	project, release := labels[LabelEngineProject], labels[LabelEngineInstance]
	if project == "" {
		project, _, _ = unstructured.NestedString(obj.Object, projectPath...)
	}
	if release == "" && releasePath != nil {
		release, _, _ = unstructured.NestedString(obj.Object, releasePath...)
	}
	if release == "" {
		release = obj.GetName()
	}
	return project, release
}

func projectSelector(project string) metav1.ListOptions {
	return metav1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s", LabelEngineProject, project)}
}

// --- Flux ---

var helmReleaseGVR = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}

type fluxAdapter struct {
	client    dynamic.Interface
	namespace string
}

// NewFluxAdapter reads the HelmReleases of namespace (okdp-releases).
func NewFluxAdapter(client dynamic.Interface, namespace string) EngineAdapter {
	return &fluxAdapter{client: client, namespace: namespace}
}

func (a *fluxAdapter) Name() string { return "flux" }

func (a *fluxAdapter) ReleaseOf(obj *unstructured.Unstructured) (string, string) {
	return releaseOf(obj, []string{"spec", "targetNamespace"}, []string{"spec", "releaseName"})
}

func (a *fluxAdapter) List(ctx context.Context, project string) (map[string]EngineStatus, error) {
	list, err := a.client.Resource(helmReleaseGVR).Namespace(a.namespace).List(ctx, projectSelector(project))
	if err != nil {
		return nil, err
	}
	out := map[string]EngineStatus{}
	for i := range list.Items {
		p, release := a.ReleaseOf(&list.Items[i])
		if p != project {
			continue
		}
		out[release] = FluxStatus(&list.Items[i])
	}
	return out, nil
}

func (a *fluxAdapter) Get(ctx context.Context, project, release string) (EngineStatus, error) {
	// By contract the HelmRelease is named after the release.
	u, err := a.client.Resource(helmReleaseGVR).Namespace(a.namespace).Get(ctx, release, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return EngineStatus{}, nil
	}
	if err != nil {
		return EngineStatus{}, err
	}
	if p, _ := a.ReleaseOf(u); p != project {
		return EngineStatus{}, nil
	}
	return FluxStatus(u), nil
}

func (a *fluxAdapter) Watch(ctx context.Context) (watch.Interface, error) {
	return a.client.Resource(helmReleaseGVR).Namespace(a.namespace).Watch(ctx, metav1.ListOptions{})
}

// Flux Ready=False reasons that only mean "not yet", not a failure.
var fluxWaitingReasons = map[string]bool{"DependencyNotReady": true, "Progressing": true}

// FluxStatus maps a HelmRelease (helm.toolkit.fluxcd.io/v2) to a phase.
func FluxStatus(u *unstructured.Unstructured) EngineStatus {
	st := EngineStatus{Found: true, CreatedAt: u.GetCreationTimestamp().Time}
	history, _, _ := unstructured.NestedSlice(u.Object, "status", "history")
	installed := len(history) > 0
	if installed {
		if latest, ok := history[0].(map[string]any); ok {
			st.Revision, _ = latest["chartVersion"].(string)
		}
	}
	if st.Revision == "" {
		st.Revision, _, _ = unstructured.NestedString(u.Object, "status", "lastAttemptedRevision")
	}

	ready := condition(u, "Ready")
	stalled := condition(u, "Stalled")
	progressing := func() {
		st.Phase = PhaseInstalling
		if installed {
			st.Phase = PhaseUpdating
		}
	}
	switch {
	case stalled != nil && stalled["status"] == "True":
		st.Phase, st.Message = PhaseError, conditionMessage(stalled)
	case ready != nil && ready["status"] == "True":
		st.Phase = PhaseReady
		// The HelmRelease can still be Ready on the previous chart version
		// while the new one is fetched.
		if rev, _, _ := unstructured.NestedString(u.Object, "status", "lastAttemptedRevision"); rev != "" && installed && rev != st.Revision {
			st.Phase = PhaseUpdating
		}
	case ready != nil && ready["status"] == "False" && !fluxWaitingReasons[fmt.Sprint(ready["reason"])]:
		st.Phase, st.Message = PhaseError, conditionMessage(ready)
	default:
		progressing()
		if ready != nil {
			st.Message = conditionMessage(ready)
		}
	}
	return st
}

// --- Argo CD ---

var applicationGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

type argoAdapter struct {
	client    dynamic.Interface
	namespace string
}

// NewArgoCDAdapter reads the Applications of namespace (argocd).
func NewArgoCDAdapter(client dynamic.Interface, namespace string) EngineAdapter {
	return &argoAdapter{client: client, namespace: namespace}
}

func (a *argoAdapter) Name() string { return "argocd" }

func (a *argoAdapter) ReleaseOf(obj *unstructured.Unstructured) (string, string) {
	return releaseOf(obj, []string{"spec", "destination", "namespace"}, nil)
}

func (a *argoAdapter) List(ctx context.Context, project string) (map[string]EngineStatus, error) {
	list, err := a.client.Resource(applicationGVR).Namespace(a.namespace).List(ctx, projectSelector(project))
	if err != nil {
		return nil, err
	}
	out := map[string]EngineStatus{}
	for i := range list.Items {
		p, release := a.ReleaseOf(&list.Items[i])
		if p != project {
			continue
		}
		out[release] = ArgoCDStatus(&list.Items[i])
	}
	return out, nil
}

func (a *argoAdapter) Get(ctx context.Context, project, release string) (EngineStatus, error) {
	u, err := a.client.Resource(applicationGVR).Namespace(a.namespace).Get(ctx, release, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return EngineStatus{}, nil
	}
	if err != nil {
		return EngineStatus{}, err
	}
	if p, _ := a.ReleaseOf(u); p != project {
		return EngineStatus{}, nil
	}
	return ArgoCDStatus(u), nil
}

func (a *argoAdapter) Watch(ctx context.Context) (watch.Interface, error) {
	return a.client.Resource(applicationGVR).Namespace(a.namespace).Watch(ctx, metav1.ListOptions{})
}

// ArgoCDStatus maps an Application (argoproj.io/v1alpha1) to a phase.
func ArgoCDStatus(u *unstructured.Unstructured) EngineStatus {
	st := EngineStatus{Found: true, CreatedAt: u.GetCreationTimestamp().Time}

	// The chart source carries the version: `chart` is set on it only.
	sources, _, _ := unstructured.NestedSlice(u.Object, "spec", "sources")
	if single, found, _ := unstructured.NestedMap(u.Object, "spec", "source"); found {
		sources = append(sources, single)
	}
	for _, raw := range sources {
		if src, ok := raw.(map[string]any); ok && src["chart"] != nil {
			st.Revision, _ = src["targetRevision"].(string)
		}
	}

	history, _, _ := unstructured.NestedSlice(u.Object, "status", "history")
	installed := len(history) > 0
	syncStatus, _, _ := unstructured.NestedString(u.Object, "status", "sync", "status")
	health, _, _ := unstructured.NestedString(u.Object, "status", "health", "status")
	healthMessage, _, _ := unstructured.NestedString(u.Object, "status", "health", "message")
	opPhase, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "phase")
	opMessage, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "message")

	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := c["type"].(string); strings.HasSuffix(t, "Error") {
			st.Phase = PhaseError
			st.Message, _ = c["message"].(string)
			return st
		}
	}
	switch {
	case opPhase == "Failed" || opPhase == "Error":
		st.Phase, st.Message = PhaseError, opMessage
	case health == "Degraded":
		st.Phase, st.Message = PhaseError, healthMessage
	case syncStatus == "Synced" && health == "Healthy" && opPhase != "Running":
		st.Phase = PhaseReady
	default:
		st.Phase = PhaseInstalling
		if installed {
			st.Phase = PhaseUpdating
		}
		st.Message = healthMessage
		if opMessage != "" {
			st.Message = opMessage
		}
	}
	return st
}

// --- helpers ---

func condition(u *unstructured.Unstructured, conditionType string) map[string]any {
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, raw := range conditions {
		if c, ok := raw.(map[string]any); ok && c["type"] == conditionType {
			return c
		}
	}
	return nil
}

func conditionMessage(c map[string]any) string {
	msg, _ := c["message"].(string)
	if msg == "" {
		msg, _ = c["reason"].(string)
	}
	return msg
}
