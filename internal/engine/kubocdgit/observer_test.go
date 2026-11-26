package kubocdgit_test

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocd"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocdgit"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

func jupyterhub(project, name string) engine.Instance {
	return engine.Instance{
		Ref:        engine.InstanceRef{Project: project, Name: name},
		Service:    "jupyterhub",
		Version:    "1.0.0",
		Parameters: map[string]any{"replicas": float64(1)},
	}
}

func create(t *testing.T, e *env, in engine.Instance) engine.Operation {
	t.Helper()
	op, err := e.engine.DesiredState.CreateInstance(testCtx(t), in, engine.WriteOptions{})
	require.NoError(t, err)
	require.Equal(t, engine.OutcomeAccepted, op.Outcome, "%v", op.Problems)
	return op
}

func observe(t *testing.T, e *env, ref engine.InstanceRef) engine.InstanceView {
	t.Helper()
	view, err := e.engine.Observer.ObserveInstance(testCtx(t), ref)
	require.NoError(t, err)
	return view
}

const unlabelledRelease = `# pushed by hand, no okdp.io labels
apiVersion: kubocd.kubotal.io/v1alpha1
kind: Release
metadata:
  name: analytics
  namespace: demo
spec:
  description: Trino pushed straight to git
  package:
    repository: quay.io/okdp/packages/trino
    tag: 1.0.0
  parameters:
    workers: 2
  targetNamespace: demo
  createNamespace: false
`

func TestAReleasePushedToGitWithoutLabelsIsObserved(t *testing.T) {
	e := newEnv(t)
	rev := pushDirect(t, e.bare, map[string][]byte{storePath + "/projects/demo/releases/analytics.yaml": []byte(unlabelledRelease)}, "add trino by hand")
	e.applied(rev)
	ref := engine.InstanceRef{Project: "demo", Name: "analytics"}

	views, err := e.engine.Observer.ObserveInstances(testCtx(t), "demo")
	require.NoError(t, err)
	require.Len(t, views, 1)
	assert.Equal(t, ref, views[0].Ref)
	assert.Equal(t, "trino", views[0].Service, "derived from the package repository")
	assert.Equal(t, "analytics", views[0].EngineName)
	assert.Equal(t, engine.PhasePending, views[0].Status.Phase)
	assert.Equal(t, rev, views[0].Status.DesiredRevision)

	e.apply(t, "demo", "analytics", true)
	view := observe(t, e, ref)
	assert.Equal(t, engine.PhaseReady, view.Status.Phase)
	assert.Equal(t, rev, view.Status.AppliedRevision)
	assert.Equal(t, "analytics", e.engine.Observer.EngineName(ref), "the engine name comes from the file, not from the formula")

	desired, revision, err := e.engine.DesiredState.GetInstance(testCtx(t), ref)
	require.NoError(t, err)
	assert.Equal(t, "trino", desired.Service)
	assert.Equal(t, rev, revision)
}

func TestAReleaseOnlyInTheClusterRequiresProjectLabelOrSyncMark(t *testing.T) {
	e := newEnv(t)
	ctx := testCtx(t)
	releases := e.cluster.Resource(crd.GetReleaseGVR()).Namespace("demo")

	// Non-project Release in the namespace (like demo-secrets): ignored.
	_, err := releases.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": crd.ReleaseAPIVersion,
		"kind":       crd.ReleaseKind,
		"metadata":   map[string]any{"name": "demo-secrets", "namespace": "demo"},
		"spec":       map[string]any{"package": map[string]any{"repository": "quay.io/okdp/sandbox-dependencies/local-secrets-provider", "tag": "1.0.0"}},
		"status":     map[string]any{"phase": "READY"},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)

	// Project Release applied directly to the cluster: observed read-only.
	_, err = releases.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": crd.ReleaseAPIVersion,
		"kind":       crd.ReleaseKind,
		"metadata": map[string]any{
			"name":      "demo-legacy",
			"namespace": "demo",
			"labels":    map[string]any{crd.LabelProject: "demo"},
		},
		"spec":   map[string]any{"package": map[string]any{"repository": "quay.io/okdp/packages/superset", "tag": "0.1.0"}},
		"status": map[string]any{"phase": "READY"},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)

	// Unlabelled Release applied from git and awaiting prune after deletion in git: observed as Deleting.
	_, err = releases.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": crd.ReleaseAPIVersion,
		"kind":       crd.ReleaseKind,
		"metadata": map[string]any{
			"name":      "analytics",
			"namespace": "demo",
			"labels":    map[string]any{"kustomize.toolkit.fluxcd.io/name": "okdp"},
		},
		"spec":   map[string]any{"package": map[string]any{"repository": "quay.io/okdp/packages/trino", "tag": "1.0.0"}},
		"status": map[string]any{"phase": "READY"},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)

	views, err := e.engine.Observer.ObserveInstances(ctx, "demo")
	require.NoError(t, err)
	require.Len(t, views, 2)

	assert.Equal(t, engine.InstanceRef{Project: "demo", Name: "analytics"}, views[0].Ref)
	assert.Equal(t, "trino", views[0].Service)
	assert.True(t, views[0].Status.Deleting)
	assert.Empty(t, views[0].Status.DesiredRevision)

	assert.Equal(t, engine.InstanceRef{Project: "demo", Name: "legacy"}, views[1].Ref)
	assert.Equal(t, "superset", views[1].Service)
	assert.Equal(t, engine.PhaseReady, views[1].Status.Phase)
	assert.False(t, views[1].Status.Deleting)
	assert.Empty(t, views[1].Status.DesiredRevision)
}

func TestConfiguredFluxKustomizationIgnoresForeignSyncMarks(t *testing.T) {
	bare := newBare(t)
	store := newStore(t, bare)
	_, err := store.Bootstrap(testCtx(t))
	require.NoError(t, err)

	cluster := newCluster()
	adapter := kubocdgit.New(kubocdgit.Deps{
		Store:    store,
		Platform: &platformStub{},
		Cluster:  cluster,
		Config: kubocd.Config{
			Persistence: kubocd.PersistenceConfig{
				Mode: kubocd.PersistenceGit,
				Git: kubocd.GitConfig{
					Sync:              kubocd.SyncFlux,
					SyncNamespace:     "okdp-system",
					KustomizationName: "okdp-cp",
				},
			},
		},
		ControlPlaneContext: repository.ContextRef{Name: "control-plane", Namespace: "okdp-system"},
	})
	eng := adapter.Engine()
	ctx := testCtx(t)
	releases := cluster.Resource(crd.GetReleaseGVR()).Namespace("demo")

	// Helper Release reconciled by another Flux Kustomization: ignored.
	_, err = releases.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": crd.ReleaseAPIVersion,
		"kind":       crd.ReleaseKind,
		"metadata": map[string]any{
			"name":      "demo-secrets",
			"namespace": "demo",
			"labels": map[string]any{
				"kustomize.toolkit.fluxcd.io/name":      "sandbox-prereqs",
				"kustomize.toolkit.fluxcd.io/namespace": "flux-system",
			},
		},
		"spec": map[string]any{
			"package":    map[string]any{"repository": "quay.io/okdp/sandbox-dependencies/local-secrets-provider", "tag": "1.0.0"},
			"parameters": map[string]any{"password": "secret"},
		},
		"status": map[string]any{"phase": "READY"},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)

	// Unlabelled Release reconciled by this Control Plane's Kustomization and deleted from git: Deleting.
	_, err = releases.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": crd.ReleaseAPIVersion,
		"kind":       crd.ReleaseKind,
		"metadata": map[string]any{
			"name":      "analytics",
			"namespace": "demo",
			"labels": map[string]any{
				"kustomize.toolkit.fluxcd.io/name":      "okdp-cp",
				"kustomize.toolkit.fluxcd.io/namespace": "okdp-system",
			},
		},
		"spec":   map[string]any{"package": map[string]any{"repository": "quay.io/okdp/packages/trino", "tag": "1.0.0"}},
		"status": map[string]any{"phase": "READY"},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)

	views, err := eng.Observer.ObserveInstances(ctx, "demo")
	require.NoError(t, err)
	require.Len(t, views, 1)
	assert.Equal(t, engine.InstanceRef{Project: "demo", Name: "analytics"}, views[0].Ref)
	assert.True(t, views[0].Status.Deleting)
}

func TestTransientReleaseListFailurePreservesClusterInstancesAsUnknown(t *testing.T) {
	bare := newBare(t)
	store := newStore(t, bare)
	_, err := store.Bootstrap(testCtx(t))
	require.NoError(t, err)

	cluster := newCluster()
	var failList atomic.Bool
	cluster.PrependReactor("list", "releases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failList.Load() {
			return true, nil, errors.New("api server unavailable")
		}
		return false, nil, nil
	})
	adapter := kubocdgit.New(kubocdgit.Deps{
		Store:               store,
		Platform:            &platformStub{},
		Cluster:             cluster,
		ControlPlaneContext: repository.ContextRef{Name: "control-plane", Namespace: "okdp-system"},
		PollInterval:        30 * time.Millisecond,
	})
	eng := adapter.Engine()
	ctx := testCtx(t)

	_, err = cluster.Resource(crd.GetReleaseGVR()).Namespace("demo").Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": crd.ReleaseAPIVersion,
		"kind":       crd.ReleaseKind,
		"metadata": map[string]any{
			"name":      "demo-legacy",
			"namespace": "demo",
			"labels":    map[string]any{crd.LabelProject: "demo"},
		},
		"spec":   map[string]any{"package": map[string]any{"repository": "quay.io/okdp/packages/superset", "tag": "0.1.0"}},
		"status": map[string]any{"phase": "READY"},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)

	stream, err := eng.Observer.WatchInstances(ctx, "demo")
	require.NoError(t, err)
	first := <-stream
	require.Equal(t, engine.EventAdded, first.Type)
	require.Equal(t, engine.PhaseReady, first.Instance.Status.Phase)

	failList.Store(true)
	_, err = eng.Observer.ObserveInstance(ctx, engine.InstanceRef{Project: "demo", Name: "legacy"})
	require.Error(t, err)
	assert.NotErrorIs(t, err, engine.ErrNotFound)

	second := <-stream
	assert.Equal(t, engine.EventModified, second.Type)
	assert.Equal(t, engine.PhaseUnknown, second.Instance.Status.Phase)
	assert.Contains(t, second.Instance.Status.Message, "api server unavailable")
}

func TestACommittedInstanceIsPendingUntilTheSyncToolAppliesIt(t *testing.T) {
	e := newEnv(t)
	in := jupyterhub("demo", "jh")
	op := create(t, e, in)
	require.Len(t, e.sync.Requests(), 2, "the bootstrap and the create woke the sync tool up")
	assert.Equal(t, e.head(), e.sync.Requests()[1])

	view := observe(t, e, in.Ref)
	assert.Equal(t, engine.PhasePending, view.Status.Phase)
	assert.Equal(t, op.DesiredRevision, view.Status.DesiredRevision)
	assert.Empty(t, view.Status.AppliedRevision)
	assert.Contains(t, view.Status.Message, op.DesiredRevision)
}

func TestAnInstanceDeletedFromGitAndStillInTheClusterIsDeleting(t *testing.T) {
	e := newEnv(t)
	in := jupyterhub("demo", "jh")
	create(t, e, in)
	e.converge(t, in.Ref)

	_, err := e.engine.DesiredState.DeleteInstance(testCtx(t), in.Ref, engine.WriteOptions{})
	require.NoError(t, err)

	views, err := e.engine.Observer.ObserveInstances(testCtx(t), "demo")
	require.NoError(t, err)
	require.Len(t, views, 1)
	assert.True(t, views[0].Status.Deleting)
	assert.Empty(t, views[0].Status.DesiredRevision)
	_, _, err = e.engine.DesiredState.GetInstance(testCtx(t), in.Ref)
	assert.ErrorIs(t, err, engine.ErrNotFound)
}

func TestTheAppliedRevisionIsTheDesiredOneOnceTheSyncToolAppliedADescendant(t *testing.T) {
	e := newEnv(t)
	in := jupyterhub("demo", "jh")
	op := create(t, e, in)
	e.converge(t, in.Ref)

	// Another project commits after: the applied revision moves past the
	// instance's own commit.
	create(t, e, jupyterhub("other", "jh"))
	newer := e.head()
	require.NotEqual(t, op.DesiredRevision, newer)
	e.applied(newer)

	view := observe(t, e, in.Ref)
	assert.Equal(t, op.DesiredRevision, view.Status.DesiredRevision)
	assert.Equal(t, op.DesiredRevision, view.Status.AppliedRevision, "normalised to the revision of the file")
	assert.Equal(t, engine.PhaseReady, view.Status.Phase)
}

func TestAnUpdateNotAppliedYetIsUpdatingWithoutAppliedRevision(t *testing.T) {
	e := newEnv(t)
	in := jupyterhub("demo", "jh")
	first := create(t, e, in)
	e.converge(t, in.Ref)

	updated := in
	updated.Version = "1.1.0"
	op, err := e.engine.DesiredState.UpdateInstance(testCtx(t), updated, engine.WriteOptions{ExpectedRevision: first.DesiredRevision})
	require.NoError(t, err)
	require.Equal(t, engine.OutcomeAccepted, op.Outcome)

	view := observe(t, e, in.Ref)
	assert.Equal(t, engine.PhaseUpdating, view.Status.Phase)
	assert.Equal(t, op.DesiredRevision, view.Status.DesiredRevision)
	assert.Empty(t, view.Status.AppliedRevision)
	assert.Equal(t, "1.1.0", view.Version, "the desired version comes from git")

	// Applied by the sync tool, the Release not reconciled for its new
	// generation yet.
	e.apply(t, "demo", "demo-jh", false)
	e.applied(e.head())
	view = observe(t, e, in.Ref)
	assert.Equal(t, engine.PhaseUpdating, view.Status.Phase, "observedGeneration lags behind the generation")
	assert.Empty(t, view.Status.AppliedRevision)

	e.converge(t, in.Ref)
	view = observe(t, e, in.Ref)
	assert.Equal(t, engine.PhaseReady, view.Status.Phase)
	assert.Equal(t, op.DesiredRevision, view.Status.AppliedRevision)
}

func TestAFailedSyncIsReportedOnTheInstancesItHolds(t *testing.T) {
	e := newEnv(t)
	in := jupyterhub("demo", "jh")
	op := create(t, e, in)

	// Unattributed failure fails closed to the neutral message.
	e.setSync(gitops.SyncState{Phase: gitops.SyncFailed, Message: "kustomize build failed: bad manifest", AttemptedRevision: e.head()})
	view := observe(t, e, in.Ref)
	assert.Equal(t, engine.PhaseError, view.Status.Phase)
	assert.Equal(t, "the sync tool failed to apply the repository (check GitOps controller logs for details)", view.Status.Message)
	assert.Equal(t, op.DesiredRevision, view.Status.DesiredRevision)

	// Failure positively scoped to this project is relayed.
	e.setSync(gitops.SyncState{Phase: gitops.SyncFailed, Message: "Release/demo/demo-jh dry-run failed", AttemptedRevision: e.head()})
	view = observe(t, e, in.Ref)
	assert.Equal(t, engine.PhaseError, view.Status.Phase)
	assert.Contains(t, view.Status.Message, "Release/demo/demo-jh dry-run failed")

	e.setSync(gitops.SyncState{Phase: gitops.SyncFailed, Message: "source unreachable"})
	view = observe(t, e, in.Ref)
	assert.Equal(t, engine.PhasePending, view.Status.Phase, "a failure with no attempted revision does not blame the instance")
	assert.Equal(t, "the sync tool failed to apply the repository (check GitOps controller logs for details)", view.Status.Message)
}

func TestReadsUseTheCachedSnapshotUntilTheSyncToolReportsANewRevision(t *testing.T) {
	e := newEnv(t)
	create(t, e, jupyterhub("demo", "jh"))
	rev := pushDirect(t, e.bare, map[string][]byte{storePath + "/projects/demo/releases/analytics.yaml": []byte(unlabelledRelease)}, "add trino by hand")

	listed, err := e.engine.DesiredState.ListInstances(testCtx(t), "demo")
	require.NoError(t, err)
	assert.Len(t, listed, 1, "no fetch on a read while the cache is fresh")

	e.setSync(gitops.SyncState{Phase: gitops.SyncProgressing, FetchedRevision: rev})
	require.Eventually(t, func() bool {
		listed, err := e.engine.DesiredState.ListInstances(testCtx(t), "demo")
		return err == nil && len(listed) == 2
	}, eventually, tick)
}

func TestACommitIsAuthoredByTheCaller(t *testing.T) {
	e := newEnv(t)
	ctx := auth.NewContext(testCtx(t), &auth.Identity{Subject: "u-1", Username: "alice@example.com"})
	_, err := e.engine.DesiredState.CreateInstance(ctx, jupyterhub("demo", "jh"), engine.WriteOptions{})
	require.NoError(t, err)

	assert.Equal(t, "alice@example.com <alice@example.com> | OKDP Control Plane",
		runGit(t, e.bare, "log", "-1", "--format=%an <%ae> | %cn", "main"))
}

func TestAnUpdateKeepsTheFieldsAndCommentsItDoesNotOwn(t *testing.T) {
	e := newEnv(t)
	pushDirect(t, e.bare, map[string][]byte{storePath + "/projects/demo/releases/analytics.yaml": []byte(unlabelledRelease)}, "add trino by hand")
	ref := engine.InstanceRef{Project: "demo", Name: "analytics"}

	op, err := e.engine.DesiredState.UpdateInstance(testCtx(t), engine.Instance{
		Ref: ref, Service: "trino", Version: "1.1.0", Parameters: map[string]any{"coordinator": true},
	}, engine.WriteOptions{})
	require.NoError(t, err)
	require.Equal(t, engine.OutcomeAccepted, op.Outcome, "%v", op.Problems)

	content := showFile(t, e.bare, "projects/demo/releases/analytics.yaml")
	assert.Contains(t, content, "# pushed by hand, no okdp.io labels")
	assert.Contains(t, content, "description: Trino pushed straight to git")
	assert.Contains(t, content, "tag: 1.1.0")
	assert.Contains(t, content, "coordinator: true")
	assert.NotContains(t, content, "workers", "the parameters are replaced, not merged")
	assert.NotContains(t, content, "okdp.io/", "no label is added to a file pushed by hand")
}

func TestAMalformedExpectedRevisionIsAConflict(t *testing.T) {
	e := newEnv(t)
	in := jupyterhub("demo", "jh")
	create(t, e, in)

	op, err := e.engine.DesiredState.UpdateInstance(testCtx(t), in, engine.WriteOptions{ExpectedRevision: "3"})
	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeRejected, op.Outcome)
	assert.Equal(t, engine.CodeConflict, op.Problems[0].Code)
}

func TestARepositoryChangeNotAppliedYetIsNotReady(t *testing.T) {
	e := newEnv(t)
	pushDirect(t, e.bare, map[string][]byte{storePath + "/projects/demo/releases/analytics.yaml": []byte(unlabelledRelease)}, "add trino by hand")
	e.apply(t, "demo", "analytics", true)
	e.applied(e.head())
	ref := engine.InstanceRef{Project: "demo", Name: "analytics"}
	require.Equal(t, engine.PhaseReady, observe(t, e, ref).Status.Phase)

	moved := strings.Replace(unlabelledRelease, "quay.io/okdp/packages/trino", "registry.example/mirror/trino", 1)
	rev := pushDirect(t, e.bare, map[string][]byte{storePath + "/projects/demo/releases/analytics.yaml": []byte(moved)}, "use the mirror")
	e.applied(rev)

	view := observe(t, e, ref)
	assert.Equal(t, engine.PhaseUpdating, view.Status.Phase, "the cluster still holds the previous repository")
	assert.Empty(t, view.Status.AppliedRevision)
}

func TestRemovingTheTargetNamespaceIsNotAppliedWhileTheClusterKeepsTheOldOne(t *testing.T) {
	e := newEnv(t)
	explicit := strings.Replace(unlabelledRelease, "targetNamespace: demo", "targetNamespace: analytics-ns", 1)
	pushDirect(t, e.bare, map[string][]byte{storePath + "/projects/demo/releases/analytics.yaml": []byte(explicit)}, "add trino by hand")
	e.apply(t, "demo", "analytics", true)
	e.applied(e.head())
	ref := engine.InstanceRef{Project: "demo", Name: "analytics"}
	require.Equal(t, engine.PhaseReady, observe(t, e, ref).Status.Phase)

	defaulted := strings.Replace(unlabelledRelease, "  targetNamespace: demo\n", "", 1)
	require.NotEqual(t, unlabelledRelease, defaulted)
	e.applied(pushDirect(t, e.bare, map[string][]byte{storePath + "/projects/demo/releases/analytics.yaml": []byte(defaulted)}, "back to the default target"))

	view := observe(t, e, ref)
	assert.Equal(t, engine.PhaseUpdating, view.Status.Phase, "the default target is the Release namespace, not analytics-ns")
	assert.Empty(t, view.Status.AppliedRevision)
}

func TestAReleaseDivergingOnInsecureContextsOrProtectedIsNotApplied(t *testing.T) {
	protected := unlabelledRelease + "  protected: true\n"
	cases := map[string]struct {
		applied, committed string
		ready              bool
	}{
		"insecure package": {
			applied:   unlabelledRelease,
			committed: strings.Replace(unlabelledRelease, "    tag: 1.0.0\n", "    tag: 1.0.0\n    insecure: true\n", 1),
		},
		"another Context":        {applied: unlabelledRelease, committed: unlabelledRelease + "  contexts:\n  - name: extra\n    namespace: demo\n"},
		"protected set in git":   {applied: unlabelledRelease, committed: protected},
		"protected set to false": {applied: protected, committed: unlabelledRelease + "  protected: false\n"},
		"protected left to the package": {
			applied: protected, committed: unlabelledRelease, ready: true,
		},
		"empty list of Contexts": {applied: unlabelledRelease, committed: unlabelledRelease + "  contexts: []\n", ready: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			file := storePath + "/projects/demo/releases/analytics.yaml"
			pushDirect(t, e.bare, map[string][]byte{file: []byte(tc.applied)}, "add trino by hand")
			e.apply(t, "demo", "analytics", true)
			e.applied(e.head())
			ref := engine.InstanceRef{Project: "demo", Name: "analytics"}
			require.Equal(t, engine.PhaseReady, observe(t, e, ref).Status.Phase)

			require.NotEqual(t, tc.applied, tc.committed)
			rev := pushDirect(t, e.bare, map[string][]byte{file: []byte(tc.committed)}, "change "+name)
			e.applied(rev)

			view := observe(t, e, ref)
			if tc.ready {
				assert.Equal(t, engine.PhaseReady, view.Status.Phase, "the difference is not one the cluster has to apply")
				assert.Equal(t, rev, view.Status.AppliedRevision)
				return
			}
			assert.Equal(t, engine.PhaseUpdating, view.Status.Phase, "the cluster still holds the previous spec")
			assert.Empty(t, view.Status.AppliedRevision)
		})
	}
}

func TestAnInstanceNameThatCannotBeALabelIsRejectedBeforeAnyCommit(t *testing.T) {
	e := newEnv(t)
	head := e.head()
	for _, name := range []string{strings.Repeat("a", 64), "Upper", "dots.in.name"} {
		op, err := e.engine.DesiredState.CreateInstance(testCtx(t), engine.Instance{
			Ref: engine.InstanceRef{Project: "demo", Name: name}, Service: "jupyterhub", Version: "1.0.0",
		}, engine.WriteOptions{})

		require.NoError(t, err)
		assert.Equal(t, engine.OutcomeRejected, op.Outcome, name)
		require.Len(t, op.Problems, 1)
		assert.Contains(t, op.Problems[0].Message, "instance name")
	}
	assert.Equal(t, head, e.head(), "nothing committed")
}

func TestANewInstanceAppliedBeforeSyncReportsRevisionStaysInstalling(t *testing.T) {
	e := newEnv(t)
	prior := e.head()
	e.applied(prior)

	in := jupyterhub("demo", "jh")
	create(t, e, in)

	// Flux applied the Release, its Kustomization still reports the prior revision.
	e.apply(t, "demo", "demo-jh", false)
	view := observe(t, e, in.Ref)
	assert.Equal(t, engine.PhaseInstalling, view.Status.Phase)
	assert.Empty(t, view.Status.AppliedRevision)
}

func TestClusterOnlyReleaseUpdateAndDeleteReturnConflict(t *testing.T) {
	e := newEnv(t)
	ctx := testCtx(t)
	_, err := e.cluster.Resource(crd.GetReleaseGVR()).Namespace("demo").Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": crd.ReleaseAPIVersion,
		"kind":       crd.ReleaseKind,
		"metadata": map[string]any{
			"name":      "demo-legacy",
			"namespace": "demo",
			"labels":    map[string]any{crd.LabelProject: "demo", crd.LabelService: "superset", crd.LabelInstanceName: "legacy"},
		},
		"spec":   map[string]any{"package": map[string]any{"repository": "quay.io/okdp/packages/superset", "tag": "0.1.0"}},
		"status": map[string]any{"phase": "READY"},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)

	ref := engine.InstanceRef{Project: "demo", Name: "legacy"}
	op, err := e.engine.DesiredState.UpdateInstance(ctx, engine.Instance{Ref: ref, Service: "superset", Version: "0.2.0"}, engine.WriteOptions{})
	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeRejected, op.Outcome)
	require.NotEmpty(t, op.Problems)
	assert.Equal(t, engine.CodeConflict, op.Problems[0].Code)

	op, err = e.engine.DesiredState.DeleteInstance(ctx, ref, engine.WriteOptions{})
	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeRejected, op.Outcome)
	require.NotEmpty(t, op.Problems)
	assert.Equal(t, engine.CodeConflict, op.Problems[0].Code)
}

func TestSyncFailureMessageRedactsOtherProjects(t *testing.T) {
	e := newEnv(t)
	in := jupyterhub("demo", "jh")
	create(t, e, in)

	for _, redacted := range []string{
		"projects/secret-team/releases/db.yaml: invalid yaml",
		"projects/secret-team/releases/db.yaml invalid yaml",
		"Release/secret-team/secret-team-db dry-run failed",
		"releases/secret-team/secret-team-db dry-run failed",
		"Namespace/secret-team not found",
		`namespaces "secret-team" not found`,
		"namespace 'secret-team' is terminating",
		"Release/demo/demo-jh failed for namespace=secret-team",
		"Release/demo/demo-jh failed: secret-team",
		`Release.kubocd.kubotal.io "secret-team-db" is invalid`,
		"Kustomization okdp-system/okdp-cp dry-run failed",
		"Kustomization demo/demo-jh dry-run failed",
		"Release/demo/demo-jh and Release/secret-team/secret-team-db failed",
		"Release/demo/demo-jh failed\nRelease/secret-team/secret-team-db failed",
		"generic error without project attribution",
	} {
		e.setSync(gitops.SyncState{Phase: gitops.SyncFailed, Message: redacted, AttemptedRevision: e.head()})
		view := observe(t, e, in.Ref)
		assert.Equal(t, engine.PhaseError, view.Status.Phase)
		assert.NotContains(t, view.Status.Message, "secret-team", "must redact foreign project from %q", redacted)
		assert.NotContains(t, view.Status.Message, "okdp-system", "must redact foreign namespace from %q", redacted)
		assert.Equal(t, "the sync tool failed to apply the repository (check GitOps controller logs for details)", view.Status.Message, "must fail closed for %q", redacted)
	}

	for _, allowed := range []string{
		"Release/demo/demo-jh dry-run failed",
		"Namespace/demo not found",
		`namespaces "demo" not found`,
		"projects/demo/releases/jh.yaml invalid yaml",
	} {
		e.setSync(gitops.SyncState{Phase: gitops.SyncFailed, Message: allowed, AttemptedRevision: e.head()})
		view := observe(t, e, in.Ref)
		assert.Contains(t, view.Status.Message, allowed)
	}
}

func TestObserveSkipsConnectionListingWhenNoBindingsExist(t *testing.T) {
	e := newEnv(t)
	in := jupyterhub("demo", "jh")
	create(t, e, in)
	e.converge(t, in.Ref)

	var connLists atomic.Int32
	for _, resource := range []string{"connections", "clusterconnections"} {
		e.cluster.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			connLists.Add(1)
			return false, nil, nil
		})
	}

	views, err := e.engine.Observer.ObserveInstances(testCtx(t), "demo")
	require.NoError(t, err)
	require.Len(t, views, 1)
	assert.Equal(t, int32(0), connLists.Load(), "should not list Connections when no instance has bindings")
}
