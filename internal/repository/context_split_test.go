package repository

import (
	"context"
	"errors"
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

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

var (
	platformRef     = ContextRef{Name: "platform", Namespace: "okdp-system"}
	controlPlaneRef = ContextRef{Name: "control-plane", Namespace: "okdp-system"}
)

func contextObject(ref ContextRef, body map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kubocd.kubotal.io/v1alpha1",
		"kind":       "Context",
		"metadata":   map[string]interface{}{"name": ref.Name, "namespace": ref.Namespace},
		"spec":       map[string]interface{}{"context": body},
	}}
}

func catalogOf(title string, services ...string) map[string]interface{} {
	list := make([]interface{}, 0, len(services))
	for _, s := range services {
		list = append(list, map[string]interface{}{"name": s, "default": "1.0.0", "versions": []interface{}{"1.0.0"}})
	}
	return map[string]interface{}{
		"defaultRepository": "quay.io/" + title,
		"categories":        []interface{}{map[string]interface{}{"title": title, "services": list}},
	}
}

func platformBody() map[string]interface{} {
	return map[string]interface{}{
		"ingress":        map[string]interface{}{"suffix": "platform.example"},
		"serviceCatalog": catalogOf("platform", "spark"),
	}
}

func newSplitCluster(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{contextGVR: "ContextList"},
		objects...,
	)
}

func serviceNames(services []models.PlatformService) []string {
	names := make([]string, 0, len(services))
	for _, s := range services {
		names = append(names, s.Name)
	}
	return names
}

func getContextObject(t *testing.T, client *dynamicfake.FakeDynamicClient, ref ContextRef) *unstructured.Unstructured {
	t.Helper()
	u, err := client.Resource(contextGVR).Namespace(ref.Namespace).Get(context.Background(), ref.Name, metav1.GetOptions{})
	require.NoError(t, err)
	return u
}

func TestSplitContextReadsTheCatalogFromThePlatformContextWhileTheControlPlaneOneIsAbsent(t *testing.T) {
	ctx := context.Background()
	repo := NewSplitContextRepository(newSplitCluster(contextObject(platformRef, platformBody())), platformRef, controlPlaneRef)

	services, err := repo.GetPlatformServices(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"spark"}, serviceNames(services))
	packages, err := repo.GetPackageRepository(ctx)
	require.NoError(t, err)
	assert.Equal(t, "quay.io/platform", packages)
	suffix, err := repo.GetIngressSuffix(ctx)
	require.NoError(t, err)
	assert.Equal(t, "platform.example", suffix)
}

func TestSplitContextReadsTheCatalogFromTheControlPlaneContextAndTheRestFromThePlatformOne(t *testing.T) {
	ctx := context.Background()
	repo := NewSplitContextRepository(newSplitCluster(
		contextObject(platformRef, platformBody()),
		contextObject(controlPlaneRef, map[string]interface{}{
			"ingress":        map[string]interface{}{"suffix": "ignored.example"},
			"serviceCatalog": catalogOf("control-plane", "jupyterhub", "trino"),
		}),
	), platformRef, controlPlaneRef)

	services, err := repo.GetPlatformServices(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"jupyterhub", "trino"}, serviceNames(services))
	categories, err := repo.GetMenuCategories(ctx)
	require.NoError(t, err)
	require.Len(t, categories, 1)
	assert.Equal(t, "control-plane", categories[0].Key)
	suffix, err := repo.GetIngressSuffix(ctx)
	require.NoError(t, err)
	assert.Equal(t, "platform.example", suffix)
}

func TestSplitContextFallsBackWhenTheControlPlaneContextHasNoServiceCatalog(t *testing.T) {
	repo := NewSplitContextRepository(newSplitCluster(
		contextObject(platformRef, platformBody()),
		contextObject(controlPlaneRef, map[string]interface{}{"other": "setting"}),
	), platformRef, controlPlaneRef)

	services, err := repo.GetPlatformServices(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"spark"}, serviceNames(services))
}

func TestSplitContextDoesNotFallBackOnATransientError(t *testing.T) {
	client := newSplitCluster(contextObject(platformRef, platformBody()))
	client.PrependReactor("get", "contexts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.GetAction).GetName() == controlPlaneRef.Name {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})
	repo := NewSplitContextRepository(client, platformRef, controlPlaneRef)

	_, err := repo.GetPlatformServices(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apiserver unavailable")
}

func TestSplitContextWriterSeedsTheControlPlaneCatalogAndNeverWritesThePlatformContext(t *testing.T) {
	ctx := context.Background()
	client := newSplitCluster(
		contextObject(platformRef, platformBody()),
		contextObject(controlPlaneRef, map[string]interface{}{"other": "setting"}),
	)
	before := getContextObject(t, client, platformRef)
	writer := NewSplitContextWriterRepository(client, platformRef, controlPlaneRef)

	require.NoError(t, writer.AddPlatformService(ctx, models.PlatformService{
		Name: "trino", Category: "platform", Versions: []string{"2.0.0"}, DefaultVersion: "2.0.0",
	}))

	assert.Equal(t, before.Object, getContextObject(t, client, platformRef).Object, "the platform Context is never written")
	repo := NewSplitContextRepository(client, platformRef, controlPlaneRef)
	services, err := repo.GetPlatformServices(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"spark", "trino"}, serviceNames(services), "the seeded catalog keeps the platform services")
	packages, err := repo.GetPackageRepository(ctx)
	require.NoError(t, err)
	assert.Equal(t, "quay.io/platform", packages, "the whole serviceCatalog is seeded")
	other, _, _ := unstructured.NestedString(getContextObject(t, client, controlPlaneRef).Object, "spec", "context", "other")
	assert.Equal(t, "setting", other)
}

func TestSplitContextWriterCreatesAMissingControlPlaneContext(t *testing.T) {
	ctx := context.Background()
	client := newSplitCluster(contextObject(platformRef, platformBody()))
	before := getContextObject(t, client, platformRef)
	writer := NewSplitContextWriterRepository(client, platformRef, controlPlaneRef)

	require.NoError(t, writer.AddPlatformService(ctx, models.PlatformService{
		Name: "trino", Category: "platform", Versions: []string{"2.0.0"}, DefaultVersion: "2.0.0",
	}))

	assert.Equal(t, before.Object, getContextObject(t, client, platformRef).Object, "the platform Context is never written")
	created := getContextObject(t, client, controlPlaneRef)
	assert.Equal(t, "Context", created.GetKind())
	services, err := NewSplitContextRepository(client, platformRef, controlPlaneRef).GetPlatformServices(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"spark", "trino"}, serviceNames(services), "created from the platform catalog")
}

func TestSplitContextWriterFailsWhenTheControlPlaneContextCannotBeRead(t *testing.T) {
	client := newSplitCluster(contextObject(platformRef, platformBody()))
	client.PrependReactor("get", "contexts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.GetAction).GetName() == controlPlaneRef.Name {
			return true, nil, errors.New("connection refused")
		}
		return false, nil, nil
	})
	writer := NewSplitContextWriterRepository(client, platformRef, controlPlaneRef)

	err := writer.RemovePlatformService(context.Background(), "spark")
	require.Error(t, err)
	assert.Contains(t, err.Error(), controlPlaneRef.String())
	_, err = client.Tracker().Get(contextGVR, controlPlaneRef.Namespace, controlPlaneRef.Name)
	assert.True(t, apierrors.IsNotFound(err), "nothing created on a read that failed: %v", err)
}

func TestOneContextHoldsEverythingWhenBothRefsAreEqual(t *testing.T) {
	ctx := context.Background()
	client := newSplitCluster(contextObject(platformRef, platformBody()))
	writer := NewSplitContextWriterRepository(client, platformRef, platformRef)

	require.NoError(t, writer.RemovePlatformService(ctx, "spark"))

	services, err := NewSplitContextRepository(client, platformRef, platformRef).GetPlatformServices(ctx)
	require.NoError(t, err)
	assert.Empty(t, services)
}

func TestCatalogUpdateKeepsTheFieldsAPlatformServiceDoesNotKnow(t *testing.T) {
	categories := []interface{}{map[string]interface{}{"title": "Data", "services": []interface{}{
		map[string]interface{}{"name": "trino", "default": "1.0.0", "versions": []interface{}{"1.0.0"}, "label": "Trino", "docsUrl": "https://trino.io"},
	}}}

	updated, err := CatalogUpdate(categories, "trino", models.PlatformService{
		Name: "trino", Category: "Data", Versions: []string{"2.0.0"}, DefaultVersion: "2.0.0",
	})
	require.NoError(t, err)

	svc := updated[0].(map[string]interface{})["services"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "https://trino.io", svc["docsUrl"])
	assert.Equal(t, "2.0.0", svc["default"])
	_, hasLabel := svc["label"]
	assert.False(t, hasLabel, "a known field the update leaves empty is dropped")
}

func TestCatalogUpdateKeepsTheServiceInPlace(t *testing.T) {
	categories := []interface{}{map[string]interface{}{"title": "Data", "services": []interface{}{
		map[string]interface{}{"name": "trino", "default": "1.0.0"},
		map[string]interface{}{"name": "spark", "default": "3.5.0"},
	}}}

	updated, err := CatalogUpdate(categories, "trino", models.PlatformService{Name: "trino", Category: "Data", DefaultVersion: "2.0.0"})
	require.NoError(t, err)
	services := updated[0].(map[string]interface{})["services"].([]interface{})
	assert.Equal(t, "trino", services[0].(map[string]interface{})["name"])
	assert.Equal(t, "2.0.0", services[0].(map[string]interface{})["default"])

	moved, err := CatalogUpdate(updated, "trino", models.PlatformService{Name: "trino", Category: "SQL", DefaultVersion: "2.0.0"})
	require.NoError(t, err)
	require.Len(t, moved, 2)
	assert.Equal(t, "SQL", moved[1].(map[string]interface{})["title"])
	assert.Len(t, moved[0].(map[string]interface{})["services"], 1)
}

func TestCatalogMutationsEnforceUniquenessAndExistenceInCAS(t *testing.T) {
	ctx := context.Background()
	client := newSplitCluster(contextObject(platformRef, platformBody()))
	writer := NewSplitContextWriterRepository(client, platformRef, controlPlaneRef)

	err := writer.AddPlatformService(ctx, models.PlatformService{
		Name: "spark", Category: "platform", Versions: []string{"1.0.0"}, DefaultVersion: "1.0.0",
	})
	assert.ErrorIs(t, err, ErrCatalogEntryExists)

	err = writer.UpdatePlatformService(ctx, "missing", models.PlatformService{
		Name: "missing", Category: "platform", Versions: []string{"1.0.0"}, DefaultVersion: "1.0.0",
	})
	assert.ErrorIs(t, err, ErrCatalogEntryNotFound)

	err = writer.RemovePlatformService(ctx, "missing")
	assert.ErrorIs(t, err, ErrCatalogEntryNotFound)
}
