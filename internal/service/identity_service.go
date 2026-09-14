package service

import (
	"context"
	"fmt"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

type IdentityService interface {
	// Available reports whether the identity backend (the Keycloak Admin
	// API) is configured, so the API can say the feature is absent instead
	// of failing on every call.
	Available(ctx context.Context) bool

	// Users
	ListUsers(ctx context.Context) ([]models.User, error)
	GetUser(ctx context.Context, name string) (*models.User, error)
	CreateUser(ctx context.Context, user *models.User) error
	UpdateUser(ctx context.Context, name string, user *models.User) error
	DeleteUser(ctx context.Context, name string) error

	// Groups
	ListGroups(ctx context.Context) ([]models.Group, error)
	GetGroup(ctx context.Context, name string) (*models.Group, error)
	CreateGroup(ctx context.Context, group *models.Group) error
	UpdateGroup(ctx context.Context, name string, group *models.Group) error
	DeleteGroup(ctx context.Context, name string) error

	// Bindings
	AssignUserToGroup(ctx context.Context, user, group string) error
	RemoveUserFromGroup(ctx context.Context, user, group string) error
}

type defaultIdentityService struct {
	repo repository.IdentityRepository
}

func NewDefaultIdentityService(repo repository.IdentityRepository) IdentityService {
	return &defaultIdentityService{
		repo: repo,
	}
}

func (s *defaultIdentityService) Available(ctx context.Context) bool {
	return s.repo.Available(ctx)
}

// --- Users ---

func (s *defaultIdentityService) ListUsers(ctx context.Context) ([]models.User, error) {
	users, err := s.repo.ListUsers(ctx)
	if err != nil {
		return nil, err
	}

	// Enrich with groups
	// This might be expensive (N queries), but for Admin console, manageable.
	// Optimally, fetch all bindings once and map them.
	bindings, err := s.repo.ListGroupBindings(ctx, "")
	if err == nil {
		bindingMap := make(map[string][]string)
		for _, b := range bindings {
			bindingMap[b.User] = append(bindingMap[b.User], b.Group)
		}

		for i := range users {
			if groups, ok := bindingMap[users[i].Username]; ok {
				users[i].Groups = groups
			} else {
				users[i].Groups = []string{}
			}
		}
	}

	return users, nil
}

func (s *defaultIdentityService) GetUser(ctx context.Context, name string) (*models.User, error) {
	user, err := s.repo.GetUser(ctx, name)
	if err != nil {
		return nil, err
	}

	bindings, err := s.repo.ListGroupBindings(ctx, name)
	if err == nil {
		var groups []string
		for _, b := range bindings {
			groups = append(groups, b.Group)
		}
		user.Groups = groups
	}

	return user, nil
}

func (s *defaultIdentityService) CreateUser(ctx context.Context, user *models.User) error {
	// Credentials are managed by the identity backend (user.Password is
	// passed through and never stored by this server).
	if err := s.repo.CreateUser(ctx, user); err != nil {
		return err
	}

	// Create group memberships if groups provided
	for _, groupName := range user.Groups {
		if err := s.repo.CreateGroupBinding(ctx, user.Username, groupName); err != nil {
			return fmt.Errorf("user created but failed to bind group %s: %w", groupName, err)
		}
	}

	return nil
}

func (s *defaultIdentityService) UpdateUser(ctx context.Context, name string, user *models.User) error {
	// An empty user.Password leaves the current credential untouched; the
	// backend owns credential storage.
	if err := s.repo.UpdateUser(ctx, name, user); err != nil {
		return err
	}

	// Update Groups (Full sync)
	// 1. List current
	currentBindings, err := s.repo.ListGroupBindings(ctx, name)
	if err != nil {
		return err
	}

	currentGroupMap := make(map[string]bool)
	for _, b := range currentBindings {
		currentGroupMap[b.Group] = true
	}

	newGroupMap := make(map[string]bool)
	for _, g := range user.Groups {
		newGroupMap[g] = true
	}

	// 2. Add new
	for g := range newGroupMap {
		if !currentGroupMap[g] {
			if err := s.repo.CreateGroupBinding(ctx, name, g); err != nil {
				return fmt.Errorf("user updated but failed to bind group %s: %w", g, err)
			}
		}
	}

	// 3. Remove old
	for g := range currentGroupMap {
		if !newGroupMap[g] {
			if err := s.repo.DeleteGroupBindingByRef(ctx, name, g); err != nil {
				return fmt.Errorf("user updated but failed to unbind group %s: %w", g, err)
			}
		}
	}

	return nil
}

func (s *defaultIdentityService) DeleteUser(ctx context.Context, name string) error {
	// The backend cascades group memberships on user deletion.
	return s.repo.DeleteUser(ctx, name)
}

// --- Groups ---

func (s *defaultIdentityService) ListGroups(ctx context.Context) ([]models.Group, error) {
	return s.repo.ListGroups(ctx)
}

func (s *defaultIdentityService) GetGroup(ctx context.Context, name string) (*models.Group, error) {
	return s.repo.GetGroup(ctx, name)
}

func (s *defaultIdentityService) CreateGroup(ctx context.Context, group *models.Group) error {
	return s.repo.CreateGroup(ctx, group)
}

func (s *defaultIdentityService) UpdateGroup(ctx context.Context, name string, group *models.Group) error {
	return s.repo.UpdateGroup(ctx, name, group)
}

func (s *defaultIdentityService) DeleteGroup(ctx context.Context, name string) error {
	return s.repo.DeleteGroup(ctx, name)
}

// --- Bindings ---

func (s *defaultIdentityService) AssignUserToGroup(ctx context.Context, user, group string) error {
	return s.repo.CreateGroupBinding(ctx, user, group)
}

func (s *defaultIdentityService) RemoveUserFromGroup(ctx context.Context, user, group string) error {
	return s.repo.DeleteGroupBindingByRef(ctx, user, group)
}
