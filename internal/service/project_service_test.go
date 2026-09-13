package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service/mocks"
)

// handWritten is a project a GitOps user declared in Git: the console never
// saw it, and its namespace (created by the engine) has no okdp.io/project label.
const handWritten = "# declared by hand\nname: demo\ndescription: Demo project\n"

func newProjectServiceUnderTest(files map[string]string) (*DefaultProjectService, *mocks.ProjectNamespaceRepository, *gitops.MemoryStore) {
	namespaces := new(mocks.ProjectNamespaceRepository)
	store := gitops.NewMemoryStore(files)
	return NewDefaultProjectService(namespaces, gitops.NewDeployments(store, nil)), namespaces, store
}

// Requirement 3: a project written by hand in Git is a project for the console.
func TestProjectsWrittenInGitAreListedAndResolved(t *testing.T) {
	svc, _, _ := newProjectServiceUnderTest(map[string]string{
		"projects/demo/project.yaml":         handWritten,
		"projects/dcr/project.yaml":          "name: dcr\n",
		"projects/dcr/services/trino/x.yaml": "x: 1\n",
		"projects/no-file/services/a/x.yaml": "x: 1\n",
		"platform/components/00-crds/x.yaml": "x: 1\n",
	})
	ctx := context.Background()

	projects, err := svc.ListProjects(ctx)
	require.NoError(t, err)
	assert.Equal(t, []models.Project{{Name: "dcr"}, {Name: "demo", Description: "Demo project"}}, projects)

	project, err := svc.GetProject(ctx, "demo")
	require.NoError(t, err)
	assert.Equal(t, &models.Project{Name: "demo", Description: "Demo project"}, project)

	for _, name := range []string{"no-file", "kube-system", "Bad_Name"} {
		_, err = svc.GetProject(ctx, name)
		assert.True(t, apierrors.IsNotFound(err), "%s: got %v", name, err)
	}
}

func TestListProjectsIsEmptyNotNull(t *testing.T) {
	svc, _, _ := newProjectServiceUnderTest(nil)
	projects, err := svc.ListProjects(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, projects)
	assert.Empty(t, projects)
}

func TestCreateProjectCommitsProjectYamlAndCreatesTheNamespace(t *testing.T) {
	svc, namespaces, store := newProjectServiceUnderTest(nil)
	ctx := aliceContext()
	project := &models.Project{Name: "demo", Description: "Demo project"}

	namespaces.On("CreateNamespace", ctx, project).Return(true, nil)
	require.NoError(t, svc.CreateProject(ctx, project))
	assert.Equal(t, "name: demo\ndescription: Demo project\n", store.Files()["projects/demo/project.yaml"])
	assert.Equal(t, "okdp: create project demo by alice"+coAuthor, store.Messages[len(store.Messages)-1])
	namespaces.AssertExpectations(t)
}

func TestCreateProjectRefusesAProjectAlreadyInGit(t *testing.T) {
	svc, namespaces, store := newProjectServiceUnderTest(map[string]string{"projects/demo/project.yaml": handWritten})
	ctx := context.Background()
	project := &models.Project{Name: "demo"}

	// The engine created the namespace of the hand-written project: it has
	// no label, so the namespace answers first.
	namespaces.On("CreateNamespace", ctx, project).Return(false, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "namespaces"}, "demo")).Once()
	err := svc.CreateProject(ctx, project)
	assert.True(t, apierrors.IsAlreadyExists(err), "got %v", err)

	// A namespace the console left behind is reused: Git answers.
	namespaces.On("CreateNamespace", ctx, project).Return(false, nil).Once()
	err = svc.CreateProject(ctx, project)
	assert.True(t, apierrors.IsAlreadyExists(err), "got %v", err)

	assert.Equal(t, handWritten, store.Files()["projects/demo/project.yaml"], "untouched")
	namespaces.AssertExpectations(t)
}

func TestCreateProjectRemovesTheNamespaceWhenGitRefuses(t *testing.T) {
	svc, namespaces, _ := newProjectServiceUnderTest(map[string]string{"projects/demo/project.yaml": handWritten})
	ctx := context.Background()
	project := &models.Project{Name: "demo"}

	namespaces.On("CreateNamespace", ctx, project).Return(true, nil)
	namespaces.On("DeleteNamespace", ctx, "demo").Return(true, nil)
	err := svc.CreateProject(ctx, project)
	assert.True(t, apierrors.IsAlreadyExists(err), "got %v", err)
	namespaces.AssertExpectations(t)
}

func TestCreateProjectRefusesAnInvalidName(t *testing.T) {
	svc, namespaces, store := newProjectServiceUnderTest(nil)
	err := svc.CreateProject(context.Background(), &models.Project{Name: "Not_A_Label"})
	assert.True(t, apierrors.IsBadRequest(err), "got %v", err)
	assert.Empty(t, store.Messages)
	namespaces.AssertNotCalled(t, "CreateNamespace")
}

func TestCreateProjectStopsOnANamespaceError(t *testing.T) {
	svc, namespaces, store := newProjectServiceUnderTest(nil)
	ctx := context.Background()
	project := &models.Project{Name: "demo"}

	namespaces.On("CreateNamespace", ctx, project).Return(false, errors.New("forbidden"))
	assert.EqualError(t, svc.CreateProject(ctx, project), "forbidden")
	assert.Empty(t, store.Messages)
}

func TestUpdateProjectWritesTheDescriptionOfAHandWrittenProject(t *testing.T) {
	svc, _, store := newProjectServiceUnderTest(map[string]string{"projects/demo/project.yaml": handWritten})
	ctx := context.Background()

	updated, err := svc.UpdateProject(ctx, &models.Project{Name: "demo", Description: "Edited"})
	require.NoError(t, err)
	assert.Equal(t, &models.Project{Name: "demo", Description: "Edited"}, updated)
	assert.Equal(t, "# declared by hand\nname: demo\ndescription: Edited\n", store.Files()["projects/demo/project.yaml"])

	_, err = svc.UpdateProject(ctx, &models.Project{Name: "missing", Description: "x"})
	assert.True(t, apierrors.IsNotFound(err), "got %v", err)
}

// Deleting removes projects/<p>/ from Git, so the engine uninstalls every
// release, then the namespace if the console created it.
func TestDeleteProjectRemovesItFromGitThenTheNamespace(t *testing.T) {
	svc, namespaces, store := newProjectServiceUnderTest(map[string]string{
		"projects/demo/project.yaml":                handWritten,
		"projects/demo/services/hive/instance.yaml": "name: hive\n",
	})
	ctx := context.Background()

	namespaces.On("DeleteNamespace", ctx, "demo").Return(false, errors.New("the cluster is away")).Once()
	assert.NoError(t, svc.DeleteProject(ctx, "demo"), "gone from Git is what counts")
	for p := range store.Files() {
		assert.NotContains(t, p, "projects/demo/")
	}

	err := svc.DeleteProject(ctx, "demo")
	assert.True(t, apierrors.IsNotFound(err), "got %v", err)
	namespaces.AssertExpectations(t)
}

func TestWatchProjectsStreamsTheGitChanges(t *testing.T) {
	svc, namespaces, _ := newProjectServiceUnderTest(map[string]string{"projects/demo/project.yaml": handWritten})
	svc.PollInterval = time.Hour // only the console's own changes wake the stream
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := svc.WatchProjects(ctx)
	require.NoError(t, err)
	next := func() ProjectEvent {
		t.Helper()
		select {
		case e := <-events:
			return e
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
			return ProjectEvent{}
		}
	}
	assert.Equal(t, ProjectEvent{Type: "ADDED", Object: models.Project{Name: "demo", Description: "Demo project"}}, next())

	namespaces.On("CreateNamespace", ctx, &models.Project{Name: "sales"}).Return(true, nil)
	require.NoError(t, svc.CreateProject(ctx, &models.Project{Name: "sales"}))
	assert.Equal(t, ProjectEvent{Type: "ADDED", Object: models.Project{Name: "sales"}}, next())

	_, err = svc.UpdateProject(ctx, &models.Project{Name: "demo", Description: "Edited"})
	require.NoError(t, err)
	assert.Equal(t, ProjectEvent{Type: "MODIFIED", Object: models.Project{Name: "demo", Description: "Edited"}}, next())

	namespaces.On("DeleteNamespace", ctx, "sales").Return(true, nil)
	require.NoError(t, svc.DeleteProject(ctx, "sales"))
	assert.Equal(t, ProjectEvent{Type: "DELETED", Object: models.Project{Name: "sales"}}, next())

	cancel()
	for range events {
	}
}
