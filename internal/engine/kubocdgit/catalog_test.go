package kubocdgit_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocd"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocdgit"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

const contextFile = "contexts/control-plane.yaml"

func names(services []models.PlatformService) []string {
	out := make([]string, 0, len(services))
	for _, s := range services {
		out = append(out, s.Name)
	}
	return out
}

func TestTheBootstrapSeedsTheControlPlaneContextFromKubernetesOnce(t *testing.T) {
	e := newEnv(t)
	content := showFile(t, e.bare, contextFile)
	assert.Contains(t, content, "kind: Context")
	assert.Contains(t, content, "name: control-plane")
	assert.Contains(t, content, "namespace: okdp-system")
	assert.Contains(t, content, "defaultRepository: quay.io/okdp/packages")

	services, err := e.engine.Catalog.Entries(testCtx(t))
	require.NoError(t, err)
	assert.Equal(t, []string{"jupyterhub", "trino"}, names(services))

	created, err := e.adapter.BootstrapControlPlaneContext(testCtx(t))
	require.NoError(t, err)
	assert.False(t, created, "the file is there, nothing is committed")
	assert.Equal(t, content, showFile(t, e.bare, contextFile))
}

func TestTheBootstrapNeverOverwritesAContextAlreadyInGit(t *testing.T) {
	bare := newBare(t)
	existing := "apiVersion: kubocd.kubotal.io/v1alpha1\nkind: Context\nmetadata:\n  name: control-plane\nspec:\n  context:\n    serviceCatalog:\n      categories: []\n"
	head := pushDirect(t, bare, map[string][]byte{storePath + "/" + contextFile: []byte(existing)}, "a Context written by hand")
	adapter := kubocdgit.New(kubocdgit.Deps{
		Store:               newStore(t, bare),
		Platform:            &platformStub{},
		Cluster:             newCluster(),
		ControlPlaneContext: repository.ContextRef{Name: "control-plane", Namespace: "okdp-system"},
		Catalog:             kubernetesCatalog,
	})

	created, err := adapter.BootstrapControlPlaneContext(testCtx(t))
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, head, runGit(t, bare, "rev-parse", "main"))
}

func TestWithoutAKubernetesCatalogTheBootstrapWritesAValidSkeleton(t *testing.T) {
	bare := newBare(t)
	store := newStore(t, bare)
	adapter := kubocdgit.New(kubocdgit.Deps{
		Store:               store,
		Platform:            &platformStub{},
		Cluster:             newCluster(),
		ControlPlaneContext: repository.ContextRef{Name: "control-plane", Namespace: "okdp-system"},
		Catalog:             func(context.Context) (map[string]any, bool, error) { return nil, false, nil },
	})

	created, err := adapter.BootstrapControlPlaneContext(testCtx(t))
	require.NoError(t, err)
	assert.True(t, created)
	assert.Contains(t, runGit(t, bare, "show", "main:"+storePath+"/"+contextFile), "categories: []")
}

func TestCatalogWritesGoToTheControlPlaneContextInGitAndKeepItsOtherFields(t *testing.T) {
	e := newEnv(t)
	ctx := testCtx(t)
	// A setting someone added by hand, unknown to the control plane.
	content := showFile(t, e.bare, contextFile) + "  # hand-written note\n  protected: true\n"
	pushDirect(t, e.bare, map[string][]byte{storePath + "/" + contextFile: []byte(content)}, "protect the Context")

	op, err := e.engine.DesiredState.CreateCatalogEntry(ctx, models.PlatformService{
		Name: "superset", Category: "Visualisation", Versions: []string{"0.1.0"}, DefaultVersion: "0.1.0",
	}, engine.WriteOptions{})
	require.NoError(t, err)
	require.Equal(t, engine.OutcomeAccepted, op.Outcome, "%v", op.Problems)
	assert.NotEmpty(t, op.DesiredRevision)

	op, err = e.engine.DesiredState.DeleteCatalogEntry(ctx, "trino", engine.WriteOptions{})
	require.NoError(t, err)
	require.Equal(t, engine.OutcomeAccepted, op.Outcome)
	op, err = e.engine.DesiredState.UpdateCatalogEntry(ctx, models.PlatformService{
		Name: "jupyterhub", Category: "Data Science", Versions: []string{"2.0.0"}, DefaultVersion: "2.0.0",
	}, engine.WriteOptions{})
	require.NoError(t, err)
	require.Equal(t, engine.OutcomeAccepted, op.Outcome)

	services, err := e.engine.Catalog.Entries(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"jupyterhub", "superset"}, names(services))
	categories, err := e.engine.Catalog.Categories(ctx)
	require.NoError(t, err)
	assert.Len(t, categories, 2)

	stored := showFile(t, e.bare, contextFile)
	assert.Contains(t, stored, "protected: true")
	assert.Contains(t, stored, "# hand-written note")
	assert.Contains(t, stored, "defaultRepository: quay.io/okdp/packages")
}

func TestPlatformSettingsAreReadFromThePlatformContext(t *testing.T) {
	e := newEnv(t)
	suffix, err := e.engine.PlatformConfig.IngressSuffix(testCtx(t))
	require.NoError(t, err)
	assert.Equal(t, "platform.example", suffix)
}

func TestTheGitContextRepositoryReadsTheCatalogFromGitAndTheRestFromThePlatform(t *testing.T) {
	e := newEnv(t)
	repo := e.adapter.ContextRepository()

	services, err := repo.GetPlatformServices(testCtx(t))
	require.NoError(t, err)
	assert.Equal(t, []string{"jupyterhub", "trino"}, names(services))
	suffix, err := repo.GetIngressSuffix(testCtx(t))
	require.NoError(t, err)
	assert.Equal(t, "platform.example", suffix)
}

func TestWithoutTheFileTheCatalogFallsBackOnThePlatformContext(t *testing.T) {
	bare := newBare(t)
	store := newStore(t, bare)
	_, err := store.Bootstrap(testCtx(t))
	require.NoError(t, err)
	adapter := kubocdgit.New(kubocdgit.Deps{
		Store:               store,
		Platform:            &platformStub{services: []models.PlatformService{{Name: "spark", Category: "Data Science", Versions: []string{"3.5.0"}, DefaultVersion: "3.5.0"}}},
		Cluster:             newCluster(),
		Config:              kubocd.Config{},
		ControlPlaneContext: repository.ContextRef{Name: "control-plane", Namespace: "okdp-system"},
		CacheTTL:            time.Hour,
	})

	services, err := adapter.Engine().Catalog.Entries(testCtx(t))
	require.NoError(t, err)
	assert.Equal(t, []string{"spark"}, names(services))

	// The first write starts the file from that catalog.
	op, err := adapter.Engine().DesiredState.CreateCatalogEntry(testCtx(t), models.PlatformService{
		Name: "trino", Category: "Data Science", Versions: []string{"1.0.0"}, DefaultVersion: "1.0.0",
	}, engine.WriteOptions{})
	require.NoError(t, err)
	require.Equal(t, engine.OutcomeAccepted, op.Outcome)
	services, err = adapter.Engine().Catalog.Entries(testCtx(t))
	require.NoError(t, err)
	assert.Equal(t, []string{"spark", "trino"}, names(services))
}

func TestTheBootstrapWritesNoSyncToolMetadataFromTheKubernetesContext(t *testing.T) {
	cluster := newCluster()
	platform := repository.ContextRef{Name: "platform", Namespace: "okdp-system"}
	controlPlane := repository.ContextRef{Name: "control-plane", Namespace: "okdp-system"}
	applied := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubocd.kubotal.io/v1alpha1",
		"kind":       "Context",
		"metadata": map[string]any{
			"name": "control-plane", "namespace": "okdp-system", "uid": "1234", "resourceVersion": "7",
			"labels":      map[string]any{"kustomize.toolkit.fluxcd.io/name": "platform"},
			"annotations": map[string]any{"kustomize.toolkit.fluxcd.io/ssa": "IfNotPresent"},
		},
		"spec": map[string]any{"context": map[string]any{"serviceCatalog": map[string]any{
			"defaultRepository": "quay.io/okdp/packages",
			"categories":        []any{map[string]any{"title": "SQL", "services": []any{map[string]any{"name": "trino", "default": "1.0.0"}}}},
		}}},
	}}
	_, err := cluster.Resource(schema.GroupVersionResource{Group: "kubocd.kubotal.io", Version: "v1alpha1", Resource: "contexts"}).
		Namespace("okdp-system").Create(testCtx(t), applied, metav1.CreateOptions{})
	require.NoError(t, err)
	bare := newBare(t)
	adapter := kubocdgit.New(kubocdgit.Deps{
		Store:               newStore(t, bare),
		Platform:            &platformStub{},
		Cluster:             cluster,
		ControlPlaneContext: controlPlane,
		Catalog: func(ctx context.Context) (map[string]any, bool, error) {
			return repository.ReadServiceCatalog(ctx, cluster, platform, controlPlane)
		},
	})

	created, err := adapter.BootstrapControlPlaneContext(testCtx(t))
	require.NoError(t, err)
	require.True(t, created)
	content := runGit(t, bare, "show", "main:"+storePath+"/"+contextFile)
	assert.Contains(t, content, "name: trino")
	for _, absent := range []string{"fluxcd", "IfNotPresent", "uid", "resourceVersion"} {
		assert.NotContains(t, content, absent)
	}
}

func TestAddingACatalogEntryDoesNotReindentSeededCategories(t *testing.T) {
	e := newEnv(t)
	ctx := testCtx(t)

	op, err := e.engine.DesiredState.CreateCatalogEntry(ctx, models.PlatformService{
		Name: "superset", Category: "Visualisation", Versions: []string{"0.1.0"}, DefaultVersion: "0.1.0",
	}, engine.WriteOptions{})
	require.NoError(t, err)
	require.Equal(t, engine.OutcomeAccepted, op.Outcome)

	diff := runGit(t, e.bare, "diff", "--unified=0", "main~1", "main", "--", storePath+"/"+contextFile)
	assert.NotContains(t, diff, "jupyterhub", "unchanged seeded entries are not reindented")
	assert.NotContains(t, diff, "trino", "unchanged seeded entries are not reindented")
	assert.Contains(t, diff, "superset")
}

func TestControlPlaneContextWithoutServiceCatalogFallsBackAndSeedsOnWrite(t *testing.T) {
	bare := newBare(t)
	store := newStore(t, bare)
	_, err := store.Bootstrap(testCtx(t))
	require.NoError(t, err)
	existing := "apiVersion: kubocd.kubotal.io/v1alpha1\nkind: Context\nmetadata:\n  name: control-plane\n  namespace: okdp-system\nspec:\n  context:\n    customSetting: keep-me\n"
	pushDirect(t, bare, map[string][]byte{storePath + "/" + contextFile: []byte(existing)}, "context without serviceCatalog")

	adapter := kubocdgit.New(kubocdgit.Deps{
		Store:               store,
		Platform:            &platformStub{services: []models.PlatformService{{Name: "spark", Category: "Data Science", Versions: []string{"3.5.0"}, DefaultVersion: "3.5.0"}}},
		Cluster:             newCluster(),
		Config:              kubocd.Config{},
		ControlPlaneContext: repository.ContextRef{Name: "control-plane", Namespace: "okdp-system"},
		CacheTTL:            time.Hour,
	})

	services, err := adapter.Engine().Catalog.Entries(testCtx(t))
	require.NoError(t, err)
	assert.Equal(t, []string{"spark"}, names(services))

	op, err := adapter.Engine().DesiredState.CreateCatalogEntry(testCtx(t), models.PlatformService{
		Name: "trino", Category: "Data Science", Versions: []string{"1.0.0"}, DefaultVersion: "1.0.0",
	}, engine.WriteOptions{})
	require.NoError(t, err)
	require.Equal(t, engine.OutcomeAccepted, op.Outcome)

	services, err = adapter.Engine().Catalog.Entries(testCtx(t))
	require.NoError(t, err)
	assert.Equal(t, []string{"spark", "trino"}, names(services))
	stored := runGit(t, bare, "show", "main:"+storePath+"/"+contextFile)
	assert.Contains(t, stored, "customSetting: keep-me")
}

func TestCatalogDuplicateCreateIsConflictAndMissingUpdateOrDeleteIsNotFound(t *testing.T) {
	e := newEnv(t)
	ctx := testCtx(t)

	op, err := e.engine.DesiredState.CreateCatalogEntry(ctx, models.PlatformService{
		Name: "trino", Category: "Data Science", Versions: []string{"1.0.0"}, DefaultVersion: "1.0.0",
	}, engine.WriteOptions{})
	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeRejected, op.Outcome)
	require.NotEmpty(t, op.Problems)
	assert.Equal(t, engine.CodeConflict, op.Problems[0].Code)

	_, err = e.engine.DesiredState.UpdateCatalogEntry(ctx, models.PlatformService{
		Name: "missing", Category: "Data Science", Versions: []string{"1.0.0"}, DefaultVersion: "1.0.0",
	}, engine.WriteOptions{})
	assert.ErrorIs(t, err, engine.ErrNotFound)

	_, err = e.engine.DesiredState.DeleteCatalogEntry(ctx, "missing", engine.WriteOptions{})
	assert.ErrorIs(t, err, engine.ErrNotFound)
}
