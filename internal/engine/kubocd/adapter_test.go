package kubocd_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/enginetest"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocd"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

type contextStub struct {
	repository.ContextRepository
}

func (contextStub) GetPlatformServices(context.Context) ([]models.PlatformService, error) {
	return []models.PlatformService{{Name: "jupyterhub", Versions: []string{"1.0.0"}, DefaultVersion: "1.0.0"}}, nil
}

func (contextStub) GetPackageRepository(context.Context) (string, error) {
	return "quay.io/okdp/packages", nil
}

// newCluster is a fake API server that keeps metadata.generation the way a
// real one does for a custom resource with a status subresource: 1 on create,
// incremented when the spec changes, untouched otherwise. The fake tracker
// alone never sets it.
func newCluster() *dynamicfake.FakeDynamicClient {
	cluster := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		crd.GetReleaseGVR():           "ReleaseList",
		crd.GetHelmReleaseGVR():       "HelmReleaseList",
		crd.GetConnectionGVR():        "ConnectionList",
		crd.GetClusterConnectionGVR(): "ClusterConnectionList",
	})
	gvr := crd.GetReleaseGVR()
	cluster.PrependReactor("create", "releases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		obj.SetGeneration(1)
		return false, nil, nil
	})
	cluster.PrependReactor("update", "releases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		stored, err := cluster.Tracker().Get(gvr, obj.GetNamespace(), obj.GetName())
		if err != nil {
			return false, nil, nil
		}
		previous := stored.(*unstructured.Unstructured)
		obj.SetGeneration(previous.GetGeneration())
		if !reflect.DeepEqual(previous.Object["spec"], obj.Object["spec"]) {
			obj.SetGeneration(previous.GetGeneration() + 1)
		}
		return false, nil, nil
	})
	return cluster
}

func newAdapter(cluster *dynamicfake.FakeDynamicClient) engine.Engine {
	return kubocd.New(kubocd.Deps{
		Releases: repository.NewServiceRepository(cluster),
		Context:  contextStub{},
		Cluster:  cluster,
		Config:   kubocd.Config{ReleaseInterval: "30m", ReleaseTimeout: "10m"},
	})
}

// converge plays the KuboCD controller: it marks the Release reconciled.
func converge(t *testing.T, cluster *dynamicfake.FakeDynamicClient, ref engine.InstanceRef) {
	t.Helper()
	ctx := context.Background()
	releases := cluster.Resource(crd.GetReleaseGVR()).Namespace(ref.Project)
	release, err := releases.Get(ctx, ref.Project+"-"+ref.Name, metav1.GetOptions{})
	if err != nil {
		return // deleted: nothing left to reconcile
	}
	require.NoError(t, unstructured.SetNestedField(release.Object, "READY", "status", "phase"))
	_, err = releases.Update(ctx, release, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func TestKuboCDAdapterHonoursTheContract(t *testing.T) {
	enginetest.Run(t, func(t *testing.T) enginetest.Harness {
		cluster := newCluster()
		return enginetest.Harness{
			Engine:   newAdapter(cluster),
			Converge: func(t *testing.T, ref engine.InstanceRef) { converge(t, cluster, ref) },
		}
	})
}

func TestTheDesiredRevisionIsTheReleaseGeneration(t *testing.T) {
	cluster := newCluster()
	eng := newAdapter(cluster)
	ctx := context.Background()
	in := engine.Instance{Ref: engine.InstanceRef{Project: "demo", Name: "jh"}, Service: "jupyterhub", Version: "1.0.0"}

	op, err := eng.DesiredState.CreateInstance(ctx, in, engine.WriteOptions{})
	require.NoError(t, err)
	assert.Equal(t, "1", op.DesiredRevision)

	in.Version = "1.1.0"
	op, err = eng.DesiredState.UpdateInstance(ctx, in, engine.WriteOptions{ExpectedRevision: "1"})
	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeAccepted, op.Outcome)
	assert.Equal(t, "2", op.DesiredRevision)

	view, err := eng.Observer.ObserveInstance(ctx, in.Ref)
	require.NoError(t, err)
	assert.Equal(t, "2", view.Status.DesiredRevision)
	assert.Empty(t, view.Status.PublishedRevision, "KuboCD has no build step")
	assert.Empty(t, view.Status.AppliedRevision, "a Release status carries no observedGeneration")
}

var jh = engine.Instance{Ref: engine.InstanceRef{Project: "demo", Name: "jh"}, Service: "jupyterhub", Version: "1.0.0"}

func failWith(cluster *dynamicfake.FakeDynamicClient, verb string, err error) {
	cluster.PrependReactor(verb, "releases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, err
	})
}

func TestARequestTheAPIServerRefusesIsRejected(t *testing.T) {
	cluster := newCluster()
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: "kubocd.kubotal.io", Kind: "Release"}, "demo-jh", nil)
	failWith(cluster, "create", invalid)

	op, err := newAdapter(cluster).DesiredState.CreateInstance(context.Background(), jh, engine.WriteOptions{})

	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeRejected, op.Outcome)
	assert.Equal(t, engine.CodeRejected, op.Problems[0].Code)
	assert.Equal(t, invalid.Error(), op.Problems[0].Message)
}

func TestAWriteThatTimedOutHasAnUnknownOutcome(t *testing.T) {
	cluster := newCluster()
	eng := newAdapter(cluster)
	ctx := context.Background()
	_, err := eng.DesiredState.CreateInstance(ctx, jh, engine.WriteOptions{})
	require.NoError(t, err)
	failWith(cluster, "update", apierrors.NewServerTimeout(crd.GetReleaseGVR().GroupResource(), "update", 1))

	op, err := eng.DesiredState.UpdateInstance(ctx, jh, engine.WriteOptions{})

	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeUnknown, op.Outcome)
	assert.True(t, strings.HasPrefix(op.Message, "failed to update release: "), op.Message)
}

func TestADeletionRacingAnUpdateIsAConflict(t *testing.T) {
	cluster := newCluster()
	eng := newAdapter(cluster)
	ctx := context.Background()
	created, err := eng.DesiredState.CreateInstance(ctx, jh, engine.WriteOptions{})
	require.NoError(t, err)
	// The Release changes between the revision check and the deletion: the API
	// server refuses the deletion precondition.
	failWith(cluster, "delete", apierrors.NewConflict(crd.GetReleaseGVR().GroupResource(), "demo-jh", nil))

	op, err := eng.DesiredState.DeleteInstance(ctx, jh.Ref, engine.WriteOptions{ExpectedRevision: created.DesiredRevision})

	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeRejected, op.Outcome)
	assert.Equal(t, engine.CodeConflict, op.Problems[0].Code)
}

func TestADeletionCarriesTheResourceVersionItChecked(t *testing.T) {
	cluster := newCluster()
	eng := newAdapter(cluster)
	ctx := context.Background()
	created, err := eng.DesiredState.CreateInstance(ctx, jh, engine.WriteOptions{})
	require.NoError(t, err)
	var preconditions *metav1.Preconditions
	cluster.PrependReactor("delete", "releases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		preconditions = action.(k8stesting.DeleteAction).GetDeleteOptions().Preconditions
		return false, nil, nil
	})

	_, err = eng.DesiredState.DeleteInstance(ctx, jh.Ref, engine.WriteOptions{ExpectedRevision: created.DesiredRevision})

	require.NoError(t, err)
	require.NotNil(t, preconditions)
	require.NotNil(t, preconditions.ResourceVersion)
}

func TestKuboCDIsAKubernetesEngine(t *testing.T) {
	info := newAdapter(newCluster()).Info
	assert.Equal(t, kubocd.Name, info.Name)
	assert.Equal(t, engine.PersistenceKubernetes, info.Persistence.Mode)
}

func TestAReleaseUnderDeletionIsDeleting(t *testing.T) {
	cluster := newCluster()
	eng := newAdapter(cluster)
	ctx := context.Background()
	in := engine.Instance{Ref: engine.InstanceRef{Project: "demo", Name: "jh"}, Service: "jupyterhub", Version: "1.0.0"}
	_, err := eng.DesiredState.CreateInstance(ctx, in, engine.WriteOptions{})
	require.NoError(t, err)
	releases := cluster.Resource(crd.GetReleaseGVR()).Namespace("demo")
	release, err := releases.Get(ctx, "demo-jh", metav1.GetOptions{})
	require.NoError(t, err)
	now := metav1.Now()
	release.SetDeletionTimestamp(&now)
	require.NoError(t, unstructured.SetNestedField(release.Object, "READY", "status", "phase"))
	_, err = releases.Update(ctx, release, metav1.UpdateOptions{})
	require.NoError(t, err)

	view, err := eng.Observer.ObserveInstance(ctx, in.Ref)

	require.NoError(t, err)
	assert.True(t, view.Status.Deleting)
	assert.Equal(t, engine.PhaseReady, view.Status.Phase, "the phase stays what the controller last reported")
}

func TestConnectionStatesDegradeWhenTheConnectionCRDsAreAbsent(t *testing.T) {
	cluster := newCluster()
	eng := newAdapter(cluster)
	ctx := context.Background()
	_, err := eng.DesiredState.CreateInstance(ctx, jh, engine.WriteOptions{})
	require.NoError(t, err)
	releases := cluster.Resource(crd.GetReleaseGVR()).Namespace("demo")
	release, err := releases.Get(ctx, "demo-jh", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedSlice(release.Object, []any{
		map[string]any{"kind": "Connection", "name": "pg", "namespace": "demo"},
	}, "status", "effectiveInputConnections"))
	require.NoError(t, unstructured.SetNestedSlice(release.Object, []any{
		map[string]any{"kind": "Connection", "name": "s3", "namespace": "demo"},
	}, "status", "watchedInputConnections"))
	_, err = releases.Update(ctx, release, metav1.UpdateOptions{})
	require.NoError(t, err)
	for _, resource := range []string{"connections", "clusterconnections"} {
		cluster.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "kubocd.kubotal.io", Resource: resource}, "")
		})
	}

	view, err := eng.Observer.ObserveInstance(ctx, jh.Ref)

	require.NoError(t, err)
	require.Len(t, view.Bindings, 2)
	assert.Equal(t, engine.ConnectionBound, view.Bindings[0].State, "bound is known from the Release alone")
	assert.Empty(t, view.Bindings[1].State, "nothing tells whether the waited one exists")
}

func TestAServerWithNoEngineConfigurationWritesTheSameRelease(t *testing.T) {
	cfg, err := kubocd.LoadConfig(func(string) (string, bool) { return "", false })
	require.NoError(t, err)
	cluster := newCluster()
	eng := kubocd.New(kubocd.Deps{
		Releases: repository.NewServiceRepository(cluster),
		Context:  contextStub{},
		Cluster:  cluster,
		Config:   cfg,
	})

	_, err = eng.DesiredState.CreateInstance(context.Background(), jh, engine.WriteOptions{})

	require.NoError(t, err)
	release, err := cluster.Resource(crd.GetReleaseGVR()).Namespace("demo").Get(context.Background(), "demo-jh", metav1.GetOptions{})
	require.NoError(t, err)
	pkg, _, _ := unstructured.NestedMap(release.Object, "spec", "package")
	assert.Equal(t, "30m0s", pkg["interval"])
	assert.Equal(t, "10m0s", pkg["timeout"])
	createNamespace, found, _ := unstructured.NestedBool(release.Object, "spec", "createNamespace")
	assert.True(t, found, "createNamespace is written, false, so the controller has nothing to add")
	assert.False(t, createNamespace)
}

func TestStatusOnlyResourceVersionConflictIsRetriedOnUpdateAndDelete(t *testing.T) {
	cluster := newCluster()
	eng := newAdapter(cluster)
	ctx := context.Background()

	created, err := eng.DesiredState.CreateInstance(ctx, jh, engine.WriteOptions{})
	require.NoError(t, err)

	// Fail the first Update with Conflict while generation stays unchanged: must retry and succeed.
	updateConflicts := 0
	cluster.PrependReactor("update", "releases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if updateConflicts == 0 {
			updateConflicts++
			return true, nil, apierrors.NewConflict(crd.GetReleaseGVR().GroupResource(), "demo-jh", nil)
		}
		return false, nil, nil
	})
	updated := jh
	updated.Version = "1.1.0"
	op, err := eng.DesiredState.UpdateInstance(ctx, updated, engine.WriteOptions{ExpectedRevision: created.DesiredRevision})
	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeAccepted, op.Outcome)
	assert.Equal(t, "2", op.DesiredRevision)
	assert.Equal(t, 1, updateConflicts)

	// Fail the first Delete with Conflict while generation stays unchanged: must retry and succeed.
	deleteConflicts := 0
	cluster.PrependReactor("delete", "releases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if deleteConflicts == 0 {
			deleteConflicts++
			return true, nil, apierrors.NewConflict(crd.GetReleaseGVR().GroupResource(), "demo-jh", nil)
		}
		return false, nil, nil
	})
	op, err = eng.DesiredState.DeleteInstance(ctx, jh.Ref, engine.WriteOptions{ExpectedRevision: op.DesiredRevision})
	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeAccepted, op.Outcome)
	assert.Equal(t, 1, deleteConflicts)
}

func TestReleaseViewPropagatesObservedGenerationAndStatusMessage(t *testing.T) {
	cluster := newCluster()
	eng := newAdapter(cluster)
	ctx := context.Background()

	_, err := eng.DesiredState.CreateInstance(ctx, jh, engine.WriteOptions{})
	require.NoError(t, err)
	releases := cluster.Resource(crd.GetReleaseGVR()).Namespace("demo")
	release, err := releases.Get(ctx, "demo-jh", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(release.Object, int64(1), "status", "observedGeneration"))
	require.NoError(t, unstructured.SetNestedField(release.Object, "waiting for dependencies", "status", "message"))
	_, err = releases.Update(ctx, release, metav1.UpdateOptions{})
	require.NoError(t, err)

	view, err := eng.Observer.ObserveInstance(ctx, jh.Ref)
	require.NoError(t, err)
	assert.Equal(t, "1", view.Status.AppliedRevision)
	assert.Equal(t, "waiting for dependencies", view.Status.Message)
}
