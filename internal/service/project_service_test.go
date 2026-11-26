package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func TestListProjects(t *testing.T) {
	mockRepo := new(mocks.ProjectRepository)
	service := NewDefaultProjectService(mockRepo)

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
	service := NewDefaultProjectService(mockRepo)

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
	service := NewDefaultProjectService(mockRepo)

	ctx := context.Background()
	newProject := &models.Project{Name: "proj1", Description: "desc1"}

	mockRepo.On("Create", ctx, newProject).Return(nil)

	err := service.CreateProject(ctx, newProject)

	assert.NoError(t, err)
	mockRepo.AssertExpectations(t)
}

func TestDeleteProject(t *testing.T) {
	mockRepo := new(mocks.ProjectRepository)
	service := NewDefaultProjectService(mockRepo)

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
	service := NewDefaultProjectService(mockRepo)

	ctx := context.Background()
	projectToDelete := "proj1"

	mockRepo.On("Delete", ctx, projectToDelete).Return(errors.New("ns delete error"))

	err := service.DeleteProject(ctx, projectToDelete)

	assert.Error(t, err)
	assert.Equal(t, "ns delete error", err.Error())
}

func TestProjectDescriptionLengthIsBounded(t *testing.T) {
	mockRepo := new(mocks.ProjectRepository)
	svc := NewDefaultProjectService(mockRepo)
	ctx := context.Background()

	atLimit := &models.Project{Name: "proj1", Description: strings.Repeat("a", maxProjectDescriptionBytes)}
	mockRepo.On("Create", ctx, atLimit).Return(nil).Once()
	require.NoError(t, svc.CreateProject(ctx, atLimit))

	mockRepo.On("Update", ctx, atLimit).Return(atLimit, nil).Once()
	updated, err := svc.UpdateProject(ctx, atLimit)
	require.NoError(t, err)
	assert.Equal(t, atLimit, updated)

	tooLong := &models.Project{Name: "proj1", Description: strings.Repeat("a", maxProjectDescriptionBytes+1)}
	err = svc.CreateProject(ctx, tooLong)
	require.Error(t, err)
	assert.True(t, apierrors.IsBadRequest(err), "must return 400 Bad Request: %v", err)

	_, err = svc.UpdateProject(ctx, tooLong)
	require.Error(t, err)
	assert.True(t, apierrors.IsBadRequest(err), "must return 400 Bad Request: %v", err)

	mockRepo.AssertExpectations(t)
}
