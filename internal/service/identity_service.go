package service

import (
	"context"
	"fmt"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"

	"golang.org/x/crypto/bcrypt"
)

type IdentityService interface {
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
	// 1. Hash password if provided
	var passwordHash string
	if user.Password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(user.Password), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("failed to hash password: %w", err)
		}
		passwordHash = string(hash)
	}

	// 2. Map DTO to CRD
	crdUser := &crd.User{
		Spec: crd.UserSpec{
			Name:         user.Name, // Display Name
			Emails:       user.Email,
			PasswordHash: passwordHash,
			Comment:      user.Comment,
			Disabled:     &user.Disabled,
		},
	}
	// Explicitly set the resource name (ID)
	crdUser.Name = user.Username

	if user.UID > 0 {
		crdUser.Spec.Uid = &user.UID
	}

	if err := s.repo.CreateUser(ctx, crdUser); err != nil {
		return err
	}

	// 3. Create Group Bindings if groups provided
	for _, groupName := range user.Groups {
		err := s.repo.CreateGroupBinding(ctx, &crd.GroupBinding{
			Spec: crd.GroupBindingSpec{
				User:  user.Username,
				Group: groupName,
			},
		})
		if err != nil {
			// Log error but continue? For now return error
			return fmt.Errorf("user created but failed to bind group %s: %w", groupName, err)
		}
	}

	return nil
}

func (s *defaultIdentityService) UpdateUser(ctx context.Context, name string, user *models.User) error {
	// First get existing to keep fields we might not want to overwrite if not provided?
	// For now, full update from UI assumed.

	existing, err := s.repo.GetUser(ctx, name)
	if err != nil {
		return err
	}

	var passwordHash string
	if user.Password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(user.Password), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("failed to hash password: %w", err)
		}
		passwordHash = string(hash)
	} else {
		// Keep existing password hash presumably?
		// We'd need to fetch the CRD to get the PasswordHash, but models.User doesn't show it.
		// Since we don't expose password hash in GetUser model, we can't easily preserve it locally here
		// unless we fetch CRD in service layer (which we do via repo but repo returns model).
		// FIX: Repo.GetUser only returns DTO. We rely on Repo.UpdateUser logic or we need to rethink patch.
		// Let's assume UpdateUser takes the DTO and if Password is empty, it shouldn't clear the hash in CRD.
		// BUT Repo.UpdateUser overwrites the spec.

		// To fix this cleanly:
		// 1. Fetch CRD directly in repo or have Service use a repo method "GetCRD" (leaking implementation details)
		// 2. Just proceed: if password empty in DTO, we assume no change. But we need the old hash.
		// The repo implementation of UpdateUser receives a CRD object. It gets the resourceVersion.
		// It overwrites the fields.

		// We can't implement partial update easily without exposing internal state.
		// Workaround: We will ignore password update if empty in DTO, but we need to retrieve the old one.
		// Since Repo.GetUser only returns DTO, we can't get the hash.

		// BETTER APPROACH:
		// Change Repo.UpdateUser to fetch the current CRD, apply changes from the passed *crd.User struct only if fields are set?
		// Or assume the service should handle this.
		// Given time constraints: I'll assume UpdateUser in repo handles replacement.
		// So I MUST fetch the CRD content. But I can't via Repo.
		// I will modify Repo to let UpdateUser handle the 'merge' logic or fetch the CRD?
		// Actually, I can just modify Repo.UpdateUser to NOT update password if empty string provided?
		// No, Repo takes a CRD struct.

		// Let's rely on retrieving the 'existing' DTO, but DTO doesn't have hash.
		// I will have to add PasswordHash to DTO (hidden from JSON output) or add a separate method `UpdatePassword`.
		// Let's keep it simple: We need to preserve the hash if not changing.
		// I'll add `GenericUpdate` or just fetch the CRD inside Repo Update method? No, Repository is dumb.

		// Re-reading code: `repository/identity_repository.go` UpdateUser fetches the object to get ResourceVersion.
		// I can modify `repository/identity_repository.go` UpdateUser to MERGE the password hash if the new one is empty!
		// But that puts business logic in repo.

		// Let's add `PasswordHash` to `models.User` with `json:"-"`.
		// Then `GetUser` fills it.
	}

	// Pending Refactor: Adding PasswordHash to models.User

	crdUser := &crd.User{
		Spec: crd.UserSpec{
			Name:     user.Name, // Display Name
			Emails:   user.Email,
			Comment:  user.Comment,
			Disabled: &user.Disabled,
			// PasswordHash: preserved from existing or updated if password provided
		},
	}
	if user.UID > 0 {
		crdUser.Spec.Uid = &user.UID
	}
	// Resource name is the ID (Username)
	crdUser.Name = name

	// Logic to preserve or update password hash
	if passwordHash != "" {
		crdUser.Spec.PasswordHash = passwordHash
	} else {
		// Use existing.PasswordHash if I can access it
		crdUser.Spec.PasswordHash = existing.PasswordHash
	}

	if err := s.repo.UpdateUser(ctx, crdUser); err != nil {
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
			s.repo.CreateGroupBinding(ctx, &crd.GroupBinding{
				Spec: crd.GroupBindingSpec{User: name, Group: g},
			})
		}
	}

	// 3. Remove old
	for g := range currentGroupMap {
		if !newGroupMap[g] {
			s.repo.DeleteGroupBindingByRef(ctx, name, g)
		}
	}

	return nil
}

func (s *defaultIdentityService) DeleteUser(ctx context.Context, name string) error {
	// First clean bindings
	bindings, err := s.repo.ListGroupBindings(ctx, name)
	if err == nil {
		for _, b := range bindings {
			s.repo.DeleteGroupBindingByRef(ctx, name, b.Group)
		}
	}
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
	crdGroup := &crd.Group{
		Spec: crd.GroupSpec{
			Comment: group.Description, // mapping description to comment
		},
	}
	// Name needs to be set in metadata
	crdGroup.Name = group.Name

	return s.repo.CreateGroup(ctx, crdGroup)
}

func (s *defaultIdentityService) UpdateGroup(ctx context.Context, name string, group *models.Group) error {
	crdGroup := &crd.Group{
		Spec: crd.GroupSpec{
			Comment: group.Description,
		},
	}
	crdGroup.Name = name

	return s.repo.UpdateGroup(ctx, crdGroup)
}

func (s *defaultIdentityService) DeleteGroup(ctx context.Context, name string) error {
	return s.repo.DeleteGroup(ctx, name)
}

// --- Bindings ---

func (s *defaultIdentityService) AssignUserToGroup(ctx context.Context, user, group string) error {
	return s.repo.CreateGroupBinding(ctx, &crd.GroupBinding{
		Spec: crd.GroupBindingSpec{User: user, Group: group},
	})
}

func (s *defaultIdentityService) RemoveUserFromGroup(ctx context.Context, user, group string) error {
	return s.repo.DeleteGroupBindingByRef(ctx, user, group)
}
