package kubocdgit

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocd"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

const (
	fluxKustomizationNameLabel      = "kustomize.toolkit.fluxcd.io/name"
	fluxKustomizationNamespaceLabel = "kustomize.toolkit.fluxcd.io/namespace"
)

// syncOwnership lists the marks a sync tool leaves on what it applied when no
// specific Flux Kustomization is configured.
var syncOwnership = struct{ labels, annotations []string }{
	labels:      []string{fluxKustomizationNameLabel, "argocd.argoproj.io/instance"},
	annotations: []string{"argocd.argoproj.io/tracking-id"},
}

func (a *Adapter) appliedFromGit(r *crd.Release) bool {
	gitCfg := a.config.Persistence.Git
	if gitCfg.KustomizationName != "" {
		if r.Labels[fluxKustomizationNameLabel] != gitCfg.KustomizationName {
			return false
		}
		ns := r.Labels[fluxKustomizationNamespaceLabel]
		return ns == "" || gitCfg.SyncNamespace == "" || ns == gitCfg.SyncNamespace
	}
	if gitCfg.Sync == kubocd.SyncFlux {
		return r.Labels[fluxKustomizationNameLabel] != ""
	}
	for _, l := range syncOwnership.labels {
		if r.Labels[l] != "" {
			return true
		}
	}
	for _, an := range syncOwnership.annotations {
		if r.Annotations[an] != "" {
			return true
		}
	}
	return false
}

// listReleases lists every Release of the namespace, labelled by the control
// plane or not.
func (a *Adapter) listReleases(ctx context.Context, namespace string) ([]crd.Release, error) {
	list, err := a.cluster.Resource(crd.GetReleaseGVR()).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]crd.Release, 0, len(list.Items))
	for i := range list.Items {
		var r crd.Release
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &r); err != nil {
			logrus.WithError(err).WithField("release", list.Items[i].GetName()).Warn("Skipping a Release that could not be decoded")
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// observe merges the Releases committed to git with the project Releases in
// the cluster. A cluster-only Release is observed when it carries the project
// label or this Control Plane's sync mark. listErr is returned alongside a
// partial view when cluster Releases could not be listed.
func (a *Adapter) observe(ctx context.Context, project string, includeConnections bool) ([]engine.InstanceView, error, error) {
	syncState := a.currentSync(ctx)
	snap, err := a.snapshots.current(ctx)
	if err != nil {
		return nil, nil, err
	}
	if a.behind(ctx, snap.Revision(), syncState) {
		if snap, err = a.snapshots.refresh(ctx); err != nil {
			return nil, nil, err
		}
	}
	files, err := releaseFiles(snap, project)
	if err != nil {
		return nil, nil, err
	}

	releases, listErr := a.listReleases(ctx, project)
	if listErr != nil {
		logrus.WithError(listErr).WithField("project", project).Warn("Cannot list the Releases, the instances are reported Unknown")
	}
	inCluster := make(map[string]*crd.Release, len(releases))
	for i := range releases {
		inCluster[releases[i].Name] = &releases[i]
	}

	views := make([]engine.InstanceView, 0, len(files)+len(releases))
	seen := map[engine.InstanceRef]bool{}
	for _, f := range sortedFiles(files) {
		if seen[f.ref] {
			logrus.WithField("file", f.path).Warn("Two Releases hold the same instance, keeping the first")
			continue
		}
		seen[f.ref] = true
		rev, err := a.snapshots.fileRevision(snap, f.path)
		if err != nil {
			return nil, nil, err
		}
		r := inCluster[f.release.Name]
		delete(inCluster, f.release.Name)
		var view engine.InstanceView
		switch {
		case listErr != nil:
			view = a.pendingView(f, rev)
			view.Status.Phase = engine.PhaseUnknown
			view.Status.Message = fmt.Sprintf("cannot read the Release: %v", listErr)
		case r == nil:
			view = a.pendingView(f, rev)
			a.withSyncProblem(ctx, project, &view.Status, rev, syncState)
		default:
			view = a.mergedView(ctx, f, rev, r, syncState)
		}
		views = append(views, view)
	}
	for _, r := range inCluster {
		fromGit := a.appliedFromGit(r)
		if r.Labels[crd.LabelProject] != project && !fromGit {
			continue
		}
		view := kubocd.ReleaseView(r)
		ref, service := identity(r)
		view.Ref, view.Service = ref, service
		if seen[ref] {
			continue
		}
		seen[ref] = true
		view.Status.DesiredRevision = ""
		view.Status.Deleting = r.DeletionTimestamp != nil || fromGit
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Ref.Name < views[j].Ref.Name })

	if includeConnections && hasBindings(views) {
		kubocd.FillConnectionStates(ctx, a.cluster, project, views, releases)
	}
	a.mu.Lock()
	for _, v := range views {
		a.names[v.Ref] = v.EngineName
	}
	a.mu.Unlock()
	return views, listErr, nil
}

func hasBindings(views []engine.InstanceView) bool {
	for i := range views {
		if len(views[i].Bindings) > 0 {
			return true
		}
	}
	return false
}

// behind reports whether the sync tool fetched or applied a revision the
// snapshot at revision does not contain: the branch moved since.
func (a *Adapter) behind(ctx context.Context, revision string, syncState gitops.SyncState) bool {
	for _, rev := range []string{syncState.FetchedRevision, syncState.AppliedRevision} {
		if rev == "" || rev == revision || !validRevision(rev) {
			continue
		}
		if revision == "" {
			return true
		}
		if ok, err := a.snapshots.isAncestor(ctx, rev, revision); err == nil && !ok {
			return true
		}
	}
	return false
}

// pendingView is an instance committed to git that the cluster does not
// hold yet.
func (a *Adapter) pendingView(f *releaseFile, rev string) engine.InstanceView {
	target := f.release.Spec.TargetNamespace
	if target == "" {
		target = f.ref.Project
	}
	return engine.InstanceView{
		Ref:             f.ref,
		EngineName:      f.release.Name,
		Service:         f.service,
		Version:         f.release.Spec.Package.Tag,
		Parameters:      f.release.Spec.Parameters,
		TargetNamespace: target,
		Status: engine.InstanceStatus{
			Phase:           engine.PhasePending,
			DesiredRevision: rev,
		},
	}
}

// mergedView is an instance committed to git and present in the cluster.
// The desired state comes from git, the rest from the Release.
func (a *Adapter) mergedView(ctx context.Context, f *releaseFile, rev string, r *crd.Release, syncState gitops.SyncState) engine.InstanceView {
	view := kubocd.ReleaseView(r)
	view.Ref, view.Service = f.ref, f.service
	view.Version = f.release.Spec.Package.Tag
	view.Parameters = f.release.Spec.Parameters
	if f.release.Spec.TargetNamespace != "" {
		view.TargetNamespace = f.release.Spec.TargetNamespace
	}
	view.Status = a.status(ctx, f, rev, r, syncState)
	return view
}

// status tells how far the cluster got with the file at revision rev.
func (a *Adapter) status(ctx context.Context, f *releaseFile, rev string, r *crd.Release, syncState gitops.SyncState) engine.InstanceStatus {
	st := engine.InstanceStatus{
		Phase:           kubocd.Phase(r.Status.Phase),
		EnginePhase:     r.Status.Phase,
		Deleting:        r.DeletionTimestamp != nil,
		Message:         r.Status.Message,
		DesiredRevision: rev,
	}

	// known: the sync tool reports an applied revision and its ancestry
	// could be read. Otherwise only the spec tells.
	applied, known := false, false
	if syncState.AppliedRevision != "" && validRevision(syncState.AppliedRevision) {
		ok, err := a.snapshots.isAncestor(ctx, rev, syncState.AppliedRevision)
		if err != nil {
			logrus.WithError(err).WithField("revision", rev).Debug("Cannot tell whether the revision is applied")
		}
		applied, known = ok, err == nil
	}
	specApplied := sameSpec(&f.release, r)
	notApplied := !specApplied || (known && !applied)
	// A controller that does not report observedGeneration leaves it at 0.
	reported := r.Status.ObservedGeneration > 0
	current := !reported || r.Status.ObservedGeneration >= r.Generation

	switch {
	case notApplied:
		if specApplied && (st.Phase == engine.PhasePending || st.Phase == engine.PhaseInstalling) {
			st.Phase = engine.PhaseInstalling
		} else {
			st.Phase = engine.PhaseUpdating
		}
		a.withSyncProblem(ctx, f.ref.Project, &st, rev, syncState)
	case !current:
		if st.Phase == engine.PhasePending || st.Phase == engine.PhaseInstalling {
			st.Phase = engine.PhaseInstalling
		} else {
			st.Phase = engine.PhaseUpdating
		}
	case st.Phase == engine.PhaseReady && !readyCondition(r):
		st.Phase = engine.PhaseUpdating
	}

	if applied && specApplied && reported && r.Status.ObservedGeneration == r.Generation {
		st.AppliedRevision = rev
	}
	return st
}

const neutralSyncFailureMessage = "the sync tool failed to apply the repository (check GitOps controller logs for details)"

// Patterns used by sanitizeSyncMessage to positively verify that a shared
// GitOps Kustomization error message refers exclusively to the current project
// and never leaks another project's name, namespace, or object details.
var (
	syncKindGroupRe     = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]*\.[a-z][a-z0-9.-]*\b`)
	syncNamespaceWordRe = regexp.MustCompile(`(?i)\bnamespaces?\s+("[^"]*"|\S+)`)
	syncQuotedRe        = regexp.MustCompile(`"[^"]*"`)
	syncKindIdentRe     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	syncObjNameRe       = regexp.MustCompile(`^[a-z0-9][-a-z0-9.]*$`)
	syncPathSegRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

// sanitizeSyncMessage fails closed: it returns a neutral explanation unless
// msg can be positively proven to refer exclusively to project and contains no
// references to other namespaces, projects, or objects.
func sanitizeSyncMessage(msg, project string) string {
	trimmed := strings.TrimSpace(msg)
	if trimmed == "" || project == "" {
		return neutralSyncFailureMessage
	}
	// Reject multiline messages and characters commonly used to embed arbitrary
	// object names, attributes, or YAML/error fragments ('...', =, :).
	if strings.ContainsAny(trimmed, "\r\n'=:") {
		return neutralSyncFailureMessage
	}
	if strings.Count(trimmed, `"`)%2 != 0 {
		return neutralSyncFailureMessage
	}

	matchedProject := false

	// Every token containing '/' must strictly target project (never another
	// namespace, project path, lowercase resource/ns/name, or Kustomization
	// <ns>/<name>).
	for _, tok := range strings.Fields(trimmed) {
		if !strings.Contains(tok, "/") {
			if syncKindGroupRe.MatchString(tok) {
				return neutralSyncFailureMessage
			}
			continue
		}
		if !isProjectScopedSlashToken(tok, project) {
			return neutralSyncFailureMessage
		}
		matchedProject = true
	}

	// Every "namespace(s) <name>" phrase must name project.
	nsMatches := syncNamespaceWordRe.FindAllStringSubmatchIndex(trimmed, -1)
	allowedQuoteRanges := make(map[[2]int]bool, len(nsMatches))
	for _, m := range nsMatches {
		val := trimmed[m[2]:m[3]]
		if strings.HasPrefix(val, `"`) && strings.HasSuffix(val, `"`) {
			if val[1:len(val)-1] != project {
				return neutralSyncFailureMessage
			}
			allowedQuoteRanges[[2]int{m[2], m[3]}] = true
		} else if val != project {
			return neutralSyncFailureMessage
		}
		matchedProject = true
	}

	// Reject any double-quoted string outside a verified "namespace(s) <project>"
	// phrase (such as `Kind.group "other-obj"`).
	for _, q := range syncQuotedRe.FindAllStringIndex(trimmed, -1) {
		if !allowedQuoteRanges[[2]int{q[0], q[1]}] {
			return neutralSyncFailureMessage
		}
	}

	if !matchedProject {
		return neutralSyncFailureMessage
	}
	return "the sync tool failed to apply the repository: " + trimmed
}

func isProjectScopedSlashToken(tok, project string) bool {
	parts := strings.Split(tok, "/")
	switch {
	case len(parts) == 2 && parts[0] == "Namespace" && parts[1] == project:
		return true
	case len(parts) == 3 && syncKindIdentRe.MatchString(parts[0]) && parts[1] == project && syncObjNameRe.MatchString(parts[2]):
		return true
	case len(parts) >= 3 && parts[0] == "projects" && parts[1] == project:
		for _, seg := range parts[2:] {
			if seg == "." || seg == ".." || !syncPathSegRe.MatchString(seg) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// withSyncProblem explains an instance waiting for a sync that failed. The
// failure is the instance's own when the attempted revision holds its file.
func (a *Adapter) withSyncProblem(ctx context.Context, project string, st *engine.InstanceStatus, rev string, syncState gitops.SyncState) {
	switch syncState.Phase {
	case gitops.SyncFailed:
		if syncState.Message != "" {
			logrus.WithFields(logrus.Fields{
				"project":  project,
				"revision": syncState.AttemptedRevision,
				"message":  syncState.Message,
			}).Warn("GitOps sync tool failed to apply the repository")
		}
		st.Message = sanitizeSyncMessage(syncState.Message, project)
		if syncState.AttemptedRevision != "" && validRevision(syncState.AttemptedRevision) {
			if ok, err := a.snapshots.isAncestor(ctx, rev, syncState.AttemptedRevision); err == nil && ok {
				st.Phase = engine.PhaseError
			}
		}
	case gitops.SyncSuspended:
		st.Message = "the sync tool is suspended, revision " + rev + " is not applied"
	default:
		if st.Message == "" {
			st.Message = "waiting for the sync tool to apply revision " + rev
		}
	}
}

// readyCondition is false only when the Release carries a Ready condition
// that is not True.
func readyCondition(r *crd.Release) bool {
	for _, c := range r.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == string(metav1.ConditionTrue)
		}
	}
	return true
}

// sameSpec reports whether the cluster holds the package, the target
// namespace, the Contexts, the protection and the parameters the file
// declares. The fields the API server or the controller default are not
// compared, nor the protection when the file leaves it to the package.
func sameSpec(want, got *crd.Release) bool {
	wp, gp := want.Spec.Package, got.Spec.Package
	if wp.Tag != gp.Tag || wp.Repository != gp.Repository || wp.Insecure != gp.Insecure {
		return false
	}
	if effectiveTarget(want) != effectiveTarget(got) {
		return false
	}
	if len(want.Spec.Contexts) != len(got.Spec.Contexts) ||
		(len(want.Spec.Contexts) > 0 && !reflect.DeepEqual(want.Spec.Contexts, got.Spec.Contexts)) {
		return false
	}
	if want.Spec.Protected != nil && (got.Spec.Protected == nil || *want.Spec.Protected != *got.Spec.Protected) {
		return false
	}
	return reflect.DeepEqual(normalize(want.Spec.Parameters), normalize(got.Spec.Parameters))
}

// effectiveTarget is the target namespace with the controller's default,
// the namespace of the Release.
func effectiveTarget(r *crd.Release) string {
	if r.Spec.TargetNamespace != "" {
		return r.Spec.TargetNamespace
	}
	return r.Namespace
}

func normalize(params map[string]any) any {
	if len(params) == 0 {
		return nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return params
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return params
	}
	return out
}

func (a *Adapter) ObserveInstances(ctx context.Context, project string) ([]engine.InstanceView, error) {
	views, _, err := a.observe(ctx, project, true)
	return views, err
}

func (a *Adapter) ObserveInstance(ctx context.Context, ref engine.InstanceRef) (engine.InstanceView, error) {
	views, listErr, err := a.observe(ctx, ref.Project, true)
	if err != nil {
		return engine.InstanceView{}, err
	}
	for _, v := range views {
		if v.Ref == ref {
			return v, nil
		}
	}
	if listErr != nil {
		return engine.InstanceView{}, fmt.Errorf("cannot list Releases in project %q: %w", ref.Project, listErr)
	}
	return engine.InstanceView{}, notFound("instance %q not found in project %q", ref.Name, ref.Project)
}

// EngineName is the Release the last observation found for ref, else the
// name a create gives it.
func (a *Adapter) EngineName(ref engine.InstanceRef) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if name, ok := a.names[ref]; ok {
		return name
	}
	return releaseName(ref)
}

// Workloads names the HelmReleases the Release owns.
func (a *Adapter) Workloads(ctx context.Context, namespace, name string) (engine.Workloads, error) {
	u, err := a.cluster.Resource(crd.GetReleaseGVR()).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return engine.Workloads{}, fmt.Errorf("failed to get release: %w", err)
	}
	helmReleases, err := a.cluster.Resource(crd.GetHelmReleaseGVR()).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return engine.Workloads{}, fmt.Errorf("failed to list helm releases: %w", err)
	}
	var names []string
	for _, hr := range helmReleases.Items {
		for _, owner := range hr.GetOwnerReferences() {
			if owner.UID == u.GetUID() {
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
