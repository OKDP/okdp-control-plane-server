package service

import (
	"context"
	"errors"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service/mocks"
	"github.com/stretchr/testify/assert"
)

func TestListProjects(t *testing.T) {
	mockRepo := new(mocks.ProjectRepository)
	service := NewDefaultProjectService(mockRepo, nil)

	ctx := context.Background()
	expectedProjects := []models.Project{
		{Name: "proj1", Description: "desc1"},
		{Name: "proj2", Description: "desc2"},
	}

	mockRepo.On("List", ctx).Return(expectedProjects, nil)

	projects, err := service.ListProjects(ctx)

	assert.NoError(t, err)
	assert.Equal(t, expectedProjects, projects)
	mockRepo.AssertExpectations(t)
}

func TestGetProject(t *testing.T) {
	mockRepo := new(mocks.ProjectRepository)
	service := NewDefaultProjectService(mockRepo, nil)

	ctx := context.Background()
	expectedProject := &models.Project{Name: "proj1", Description: "desc1"}

	mockRepo.On("Get", ctx, "proj1").Return(expectedProject, nil)

	project, err := service.GetProject(ctx, "proj1")

	assert.NoError(t, err)
	assert.Equal(t, expectedProject, project)
	mockRepo.AssertExpectations(t)
}

func TestCreateProject(t *testing.T) {
	mockRepo := new(mocks.ProjectRepository)
	service := NewDefaultProjectService(mockRepo, nil)

	ctx := context.Background()
	newProject := &models.Project{Name: "proj1", Description: "desc1"}

	mockRepo.On("Create", ctx, newProject).Return(nil)

	err := service.CreateProject(ctx, newProject)

	assert.NoError(t, err)
	mockRepo.AssertExpectations(t)
}

func TestDeleteProject(t *testing.T) {
	mockRepo := new(mocks.ProjectRepository)
	service := NewDefaultProjectService(mockRepo, nil)

	ctx := context.Background()
	projectToDelete := "proj1"

	// DeleteProject removes the Namespace. No per-project Context is managed.
	mockRepo.On("Delete", ctx, projectToDelete).Return(nil)

	err := service.DeleteProject(ctx, projectToDelete)

	assert.NoError(t, err)
	mockRepo.AssertExpectations(t)
}

func TestDeleteProject_RepoError(t *testing.T) {
	mockRepo := new(mocks.ProjectRepository)
	service := NewDefaultProjectService(mockRepo, nil)

	ctx := context.Background()
	projectToDelete := "proj1"

	mockRepo.On("Delete", ctx, projectToDelete).Return(errors.New("ns delete error"))

	err := service.DeleteProject(ctx, projectToDelete)

	assert.Error(t, err)
	assert.Equal(t, "ns delete error", err.Error())
}

// A project is declared in Git too: projects/<p>/project.yaml, and deleting
// it removes everything it declared, so the engine uninstalls its releases.
func TestProjectsAreDeclaredInGit(t *testing.T) {
	mockRepo := new(mocks.ProjectRepository)
	store := gitops.NewMemoryStore(nil)
	service := NewDefaultProjectService(mockRepo, gitops.NewDeployments(store, nil))
	ctx := context.Background()
	project := &models.Project{Name: "demo", Description: "Demo project"}

	mockRepo.On("Create", ctx, project).Return(nil)
	assert.NoError(t, service.CreateProject(ctx, project))
	assert.Equal(t, "name: demo\ndescription: Demo project\n", store.Files()["projects/demo/project.yaml"])

	mockRepo.On("Delete", ctx, "demo").Return(nil)
	assert.NoError(t, service.DeleteProject(ctx, "demo"))
	assert.NotContains(t, store.Files(), "projects/demo/project.yaml")
	mockRepo.AssertExpectations(t)
}
