package kubocdgit_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocdgit"
	"github.com/okdp/okdp-control-plane-server/internal/models"
)

func newProjects(t *testing.T, e *env, objects ...corev1.Namespace) (*kubocdgit.ProjectRepository, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset()
	for i := range objects {
		_, err := client.CoreV1().Namespaces().Create(testCtx(t), &objects[i], metav1.CreateOptions{})
		require.NoError(t, err)
	}
	return kubocdgit.NewProjectRepository(e.adapter, client), client
}

func TestAProjectIsANamespaceManifestInGit(t *testing.T) {
	e := newEnv(t)
	projects, _ := newProjects(t, e)
	ctx := testCtx(t)

	require.NoError(t, projects.Create(ctx, &models.Project{Name: "demo", Description: "Demo project"}))
	content := showFile(t, e.bare, "projects/demo/namespace.yaml")
	assert.Contains(t, content, "kind: Namespace")
	assert.Contains(t, content, "okdp.io/project: demo")
	assert.Contains(t, content, "okdp.io/description: Demo project")

	got, err := projects.Get(ctx, "demo")
	require.NoError(t, err)
	assert.Equal(t, &models.Project{Name: "demo", Description: "Demo project"}, got)

	err = projects.Create(ctx, &models.Project{Name: "demo"})
	assert.True(t, apierrors.IsAlreadyExists(err), "%v", err)

	// A field added by hand survives an update.
	pushDirect(t, e.bare, map[string][]byte{storePath + "/projects/demo/namespace.yaml": []byte(content + "\nspec:\n  finalizers: [kubernetes]\n")}, "hand edit")
	updated, err := projects.Update(ctx, &models.Project{Name: "demo", Description: "Renamed"})
	require.NoError(t, err)
	assert.Equal(t, "Renamed", updated.Description)
	content = showFile(t, e.bare, "projects/demo/namespace.yaml")
	assert.Contains(t, content, "okdp.io/description: Renamed")
	assert.Contains(t, content, "finalizers: [kubernetes]")
}

func TestCreatingAProjectOverAnExistingNamespaceIsRefused(t *testing.T) {
	e := newEnv(t)
	projects, _ := newProjects(t, e, corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}})

	err := projects.Create(testCtx(t), &models.Project{Name: "kube-system"})
	assert.True(t, apierrors.IsAlreadyExists(err), "%v", err)
}

func TestProjectsAreListedFromGitAndFromTheCluster(t *testing.T) {
	e := newEnv(t)
	projects, _ := newProjects(t, e, corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "legacy", Labels: map[string]string{"okdp.io/project": "legacy"},
		Annotations: map[string]string{"okdp.io/description": "From kubernetes mode"},
	}}, corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	ctx := testCtx(t)
	require.NoError(t, projects.Create(ctx, &models.Project{Name: "demo", Description: "Committed"}))

	list, err := projects.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []models.Project{{Name: "demo", Description: "Committed"}, {Name: "legacy", Description: "From kubernetes mode"}}, list)

	err = projects.Delete(ctx, "legacy")
	assert.True(t, apierrors.IsConflict(err), "a project only the cluster holds is not deleted from git: %v", err)
	err = projects.Delete(ctx, "ghost")
	assert.True(t, apierrors.IsNotFound(err), "%v", err)
}

func TestDeletingAProjectRemovesItsDirectoryInOneCommit(t *testing.T) {
	e := newEnv(t)
	projects, _ := newProjects(t, e)
	ctx := testCtx(t)
	require.NoError(t, projects.Create(ctx, &models.Project{Name: "demo"}))
	require.NoError(t, projects.Create(ctx, &models.Project{Name: "other"}))
	create(t, e, jupyterhub("demo", "jh"))
	before := e.head()

	require.NoError(t, projects.Delete(ctx, "demo"))

	assert.Equal(t, "1", runGit(t, e.bare, "rev-list", "--count", before+"..main"))
	files := runGit(t, e.bare, "ls-tree", "-r", "--name-only", "main", storePath+"/projects")
	assert.Equal(t, storePath+"/projects/other/namespace.yaml", files)
	_, err := projects.Get(ctx, "demo")
	assert.True(t, apierrors.IsNotFound(err), "%v", err)
}

func TestProjectListSkipsInvalidManifestsAndGetRefreshesOnMiss(t *testing.T) {
	e := newEnv(t)
	projects, _ := newProjects(t, e)
	ctx := testCtx(t)

	require.NoError(t, projects.Create(ctx, &models.Project{Name: "demo", Description: "Valid"}))
	// Warm the cache.
	list, err := projects.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Push invalid manifests and a valid new project directly to git while the cache is fresh.
	pushDirect(t, e.bare, map[string][]byte{
		storePath + "/projects/wrong-kind/namespace.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: wrong-kind\n  labels:\n    okdp.io/project: wrong-kind\n"),
		storePath + "/projects/wrong-name/namespace.yaml": []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: other\n  labels:\n    okdp.io/project: wrong-name\n"),
		storePath + "/projects/broken/namespace.yaml":     []byte("kind: Namespace\nmetadata: [broken\n"),
		storePath + "/projects/pushed/namespace.yaml":     []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: pushed\n  labels:\n    okdp.io/project: pushed\n  annotations:\n    okdp.io/description: Pushed out of band\n"),
	}, "add invalid and out-of-band projects")

	// Get falls back to a refresh before reporting NotFound.
	got, err := projects.Get(ctx, "pushed")
	require.NoError(t, err)
	assert.Equal(t, &models.Project{Name: "pushed", Description: "Pushed out of band"}, got)

	// List skips invalid project manifests with a warning instead of failing.
	list, err = projects.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []models.Project{
		{Name: "demo", Description: "Valid"},
		{Name: "pushed", Description: "Pushed out of band"},
	}, list)
}

func TestProjectDescriptionLimitEnforcedInGitRepository(t *testing.T) {
	e := newEnv(t)
	projects, _ := newProjects(t, e)
	ctx := testCtx(t)

	require.NoError(t, projects.Create(ctx, &models.Project{Name: "demo", Description: strings.Repeat("a", 4096)}))
	err := projects.Create(ctx, &models.Project{Name: "too-big", Description: strings.Repeat("a", 4097)})
	require.Error(t, err)
	assert.True(t, apierrors.IsBadRequest(err), "%v", err)

	_, err = projects.Update(ctx, &models.Project{Name: "demo", Description: strings.Repeat("b", 4097)})
	require.Error(t, err)
	assert.True(t, apierrors.IsBadRequest(err), "%v", err)
}
