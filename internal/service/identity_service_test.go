package service

import (
	"context"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service/mocks"
	"github.com/stretchr/testify/assert"
)

func TestListUsers(t *testing.T) {
	mockRepo := new(mocks.IdentityRepository)
	service := NewDefaultIdentityService(mockRepo)

	ctx := context.Background()
	expectedUsers := []models.User{
		{Username: "user1", Name: "User One"},
		{Username: "user2", Name: "User Two"},
	}

	mockRepo.On("ListUsers", ctx).Return(expectedUsers, nil)
	mockRepo.On("ListGroupBindings", ctx, "").Return([]models.GroupBinding{}, nil)

	users, err := service.ListUsers(ctx)

	assert.NoError(t, err)
	assert.Equal(t, expectedUsers, users)
	mockRepo.AssertExpectations(t)
}

func TestGetUser(t *testing.T) {
	mockRepo := new(mocks.IdentityRepository)
	service := NewDefaultIdentityService(mockRepo)

	ctx := context.Background()
	expectedUser := &models.User{Username: "user1", Name: "User One"}

	mockRepo.On("GetUser", ctx, "user1").Return(expectedUser, nil)
	mockRepo.On("ListGroupBindings", ctx, "user1").Return([]models.GroupBinding{}, nil)

	user, err := service.GetUser(ctx, "user1")

	assert.NoError(t, err)
	assert.Equal(t, expectedUser, user)
	mockRepo.AssertExpectations(t)
}

func TestListGroups(t *testing.T) {
	mockRepo := new(mocks.IdentityRepository)
	service := NewDefaultIdentityService(mockRepo)

	ctx := context.Background()
	expectedGroups := []models.Group{
		{Name: "group1"},
		{Name: "group2"},
	}

	mockRepo.On("ListGroups", ctx).Return(expectedGroups, nil)

	groups, err := service.ListGroups(ctx)

	assert.NoError(t, err)
	assert.Equal(t, expectedGroups, groups)
	mockRepo.AssertExpectations(t)
}

func TestDeleteUser(t *testing.T) {
	mockRepo := new(mocks.IdentityRepository)
	service := NewDefaultIdentityService(mockRepo)

	ctx := context.Background()
	username := "user1"

	// Mock deleting bindings first
	mockRepo.On("ListGroupBindings", ctx, username).Return([]models.GroupBinding{
		{User: username, Group: "g1"},
	}, nil)

	mockRepo.On("DeleteGroupBindingByRef", ctx, username, "g1").Return(nil)

	// Mock deleting user
	mockRepo.On("DeleteUser", ctx, username).Return(nil)

	err := service.DeleteUser(ctx, username)

	assert.NoError(t, err)
	mockRepo.AssertExpectations(t)
}
