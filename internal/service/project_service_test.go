package service

import (
	"context"
	"errors"
	"testing"

	"github.com/okdp/okdp-server-new/internal/models"
	"github.com/okdp/okdp-server-new/internal/service/mocks"
	"github.com/stretchr/testify/assert"
)

func TestListProjects(t *testing.T) {
	mockRepo := new(mocks.ProjectRepository)
	mockCtxRepo := new(mocks.ContextWriterRepository)
	service := NewDefaultProjectService(mockRepo, mockCtxRepo)

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
	mockCtxRepo := new(mocks.ContextWriterRepository)
	service := NewDefaultProjectService(mockRepo, mockCtxRepo)

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
	mockCtxRepo := new(mocks.ContextWriterRepository)
	service := NewDefaultProjectService(mockRepo, mockCtxRepo)

	ctx := context.Background()
	newProject := &models.Project{Name: "proj1", Description: "desc1"}

	mockRepo.On("Create", ctx, newProject).Return(nil)
	// CreateProject also provisions a per-project KuboCD Context CR cloned
	// from the default one; the write is best-effort so a nil error is fine.
	mockCtxRepo.On("CreateFromDefault", ctx, newProject.Name).Return(nil)

	err := service.CreateProject(ctx, newProject)

	assert.NoError(t, err)
	mockRepo.AssertExpectations(t)
	mockCtxRepo.AssertExpectations(t)
}

func TestDeleteProject(t *testing.T) {
	mockRepo := new(mocks.ProjectRepository)
	mockCtxRepo := new(mocks.ContextWriterRepository)
	service := NewDefaultProjectService(mockRepo, mockCtxRepo)

	ctx := context.Background()
	projectToDelete := "proj1"

	// DeleteProject removes the per-project KuboCD Context CR then the Namespace.
	mockCtxRepo.On("Delete", ctx, projectToDelete).Return(nil)
	mockRepo.On("Delete", ctx, projectToDelete).Return(nil)

	err := service.DeleteProject(ctx, projectToDelete)

	assert.NoError(t, err)
	mockRepo.AssertExpectations(t)
	mockCtxRepo.AssertExpectations(t)
}

func TestDeleteProject_RepoError(t *testing.T) {
	mockRepo := new(mocks.ProjectRepository)
	mockCtxRepo := new(mocks.ContextWriterRepository)
	service := NewDefaultProjectService(mockRepo, mockCtxRepo)

	ctx := context.Background()
	projectToDelete := "proj1"

	mockCtxRepo.On("Delete", ctx, projectToDelete).Return(nil)
	mockRepo.On("Delete", ctx, projectToDelete).Return(errors.New("ns delete error"))

	err := service.DeleteProject(ctx, projectToDelete)

	assert.Error(t, err)
	assert.Equal(t, "ns delete error", err.Error())
}
