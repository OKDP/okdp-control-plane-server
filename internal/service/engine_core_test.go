package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/fake"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
)

// These tests run the services core against the fake engine, which answers
// like a git-backed one: commits as revisions, instances Pending until it acts.

type cleanupSpy struct{ deleted []string }

func (c *cleanupSpy) EnsureClient(context.Context, provisioning.OidcClientSpec) error { return nil }
func (c *cleanupSpy) DeleteClient(_ context.Context, name string) error {
	c.deleted = append(c.deleted, name)
	return nil
}

func newCoreOnFake(t *testing.T, options ...fake.Option) (*DefaultServiceService, *fake.Engine, *cleanupSpy) {
	t.Helper()
	f := fake.New(options...)
	_, err := f.CreateCatalogEntry(context.Background(), models.PlatformService{
		Name: "jupyterhub", Versions: []string{"1.0.0", "1.1.0"}, DefaultVersion: "1.0.0", Category: "Notebooks",
	}, engine.WriteOptions{})
	require.NoError(t, err)
	cluster := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Version: "v1", Resource: "pods"}:                                  "PodList",
		{Version: "v1", Resource: "events"}:                                "EventList",
		{Version: "v1", Resource: "persistentvolumeclaims"}:                "PersistentVolumeClaimList",
		{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}: "IngressList",
	})
	spy := &cleanupSpy{}
	return NewDefaultServiceService(f.Engine(), spy, cluster, k8sfake.NewClientset(), nil), f, spy
}

var jhRef = engine.InstanceRef{Project: "demo", Name: "jh"}

func TestAnAcceptedWriteIsPendingWithItsCommit(t *testing.T) {
	core, f, _ := newCoreOnFake(t)
	ctx := context.Background()

	created, err := core.DeployService(ctx, "demo", models.ServiceRequest{Service: "jupyterhub", InstanceName: "jh", Parameters: map[string]any{}})

	require.NoError(t, err)
	assert.Equal(t, "Pending", created.Phase)
	assert.Equal(t, "Installing", created.Status)
	assert.Len(t, created.DesiredRevision, 40)
	assert.Empty(t, created.AppliedRevision)

	f.Settle(jhRef)
	got, err := core.GetService(ctx, "demo", "jh")
	require.NoError(t, err)
	assert.Equal(t, "Ready", got.Phase)
	assert.Equal(t, "Ready", got.Status)
	assert.Equal(t, created.DesiredRevision, got.AppliedRevision)
}

func TestTheThreeRevisionsDifferWhileAnUpdateIsBuiltAndApplied(t *testing.T) {
	core, f, _ := newCoreOnFake(t, fake.WithBuildStep())
	ctx := context.Background()
	first, err := core.DeployService(ctx, "demo", models.ServiceRequest{Service: "jupyterhub", InstanceName: "jh", Parameters: map[string]any{}})
	require.NoError(t, err)
	f.Settle(jhRef)
	built, err := core.GetService(ctx, "demo", "jh")
	require.NoError(t, err)

	events, err := core.WatchServices(ctx, "demo")
	require.NoError(t, err)
	updated, err := core.UpdateServiceParameters(ctx, "demo", "jh", models.ServiceUpdateRequest{Tag: "1.1.0"}, "")
	require.NoError(t, err)
	streamed := <-events

	for name, instance := range map[string]models.ServiceInstance{"update answer": *updated, "stream": streamed.Instance} {
		assert.NotEqual(t, first.DesiredRevision, instance.DesiredRevision, name)
		assert.Equal(t, built.PublishedRevision, instance.PublishedRevision, name+": the new revision is not built yet")
		assert.Equal(t, first.DesiredRevision, instance.AppliedRevision, name+": the old revision still runs")
		assert.Equal(t, "Pending", instance.Phase, name)
	}
	listed, err := core.ListServices(ctx, "demo")
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, updated.DesiredRevision, listed[0].DesiredRevision)
	assert.NotEmpty(t, listed[0].PublishedRevision)
	assert.NotEqual(t, listed[0].DesiredRevision, listed[0].PublishedRevision)
}

func TestAWriteAnsweredWithoutAViewKeepsItsRevision(t *testing.T) {
	core, _, _ := newCoreOnFake(t, fake.WithoutWriteViews())

	created, err := core.DeployService(context.Background(), "demo", models.ServiceRequest{Service: "jupyterhub", InstanceName: "jh", Parameters: map[string]any{}})

	require.NoError(t, err)
	assert.Equal(t, "Pending", created.Phase)
	assert.Len(t, created.DesiredRevision, 40)
	assert.Equal(t, "jh", created.Name)
}

func TestAnUnknownOutcomeIsNeitherSuccessNorRejection(t *testing.T) {
	core, f, _ := newCoreOnFake(t)
	f.UnknownNext()

	_, err := core.DeployService(context.Background(), "demo", models.ServiceRequest{Service: "jupyterhub", InstanceName: "jh", Parameters: map[string]any{}})

	assert.True(t, errors.Is(err, engine.ErrOutcomeUnknown))
	var rejected *engine.RejectedError
	assert.False(t, errors.As(err, &rejected))
	got, err := core.GetService(context.Background(), "demo", "jh")
	require.NoError(t, err, "the write landed after all")
	assert.Equal(t, "Pending", got.Phase)
}

func TestACommittedDeletionLeavesTheWorkloadsResourcesAlone(t *testing.T) {
	core, f, spy := newCoreOnFake(t)
	ctx := context.Background()
	_, err := core.DeployService(ctx, "demo", models.ServiceRequest{Service: "jupyterhub", InstanceName: "jh", Parameters: map[string]any{}})
	require.NoError(t, err)
	f.Settle(jhRef)

	op, err := core.DeleteService(ctx, "demo", "jh", "")
	require.NoError(t, err)
	require.NotNil(t, op, "a committed deletion answers its operation")
	assert.Equal(t, "accepted", op.Outcome)
	assert.NotEmpty(t, op.DesiredRevision)

	assert.Empty(t, spy.deleted, "the OIDC client outlives a deletion that is only committed")
	got, err := core.GetService(ctx, "demo", "jh")
	require.NoError(t, err)
	assert.Equal(t, "Deleting", got.Phase)
}
