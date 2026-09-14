package repository

import (
	"context"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// IdentityRepository abstracts the identity backend (users, groups and
// user/group memberships). Implementations translate the API models to the
// backing identity provider.
type IdentityRepository interface {
	// Available reports whether the backend is configured, so the API can say
	// the feature is absent instead of failing on every call.
	Available(ctx context.Context) bool

	// Users
	ListUsers(ctx context.Context) ([]models.User, error)
	GetUser(ctx context.Context, name string) (*models.User, error)
	// CreateUser creates the user; user.Password, when set, is the initial
	// credential (managed by the backend, never stored by this server).
	CreateUser(ctx context.Context, user *models.User) error
	// UpdateUser replaces the user profile; an empty user.Password leaves
	// the current credential untouched.
	UpdateUser(ctx context.Context, name string, user *models.User) error
	DeleteUser(ctx context.Context, name string) error

	// Groups
	ListGroups(ctx context.Context) ([]models.Group, error)
	GetGroup(ctx context.Context, name string) (*models.Group, error)
	CreateGroup(ctx context.Context, group *models.Group) error
	UpdateGroup(ctx context.Context, name string, group *models.Group) error
	DeleteGroup(ctx context.Context, name string) error

	// GroupBindings (user ↔ group memberships)
	ListGroupBindings(ctx context.Context, userFilter string) ([]models.GroupBinding, error)
	CreateGroupBinding(ctx context.Context, user, group string) error
	DeleteGroupBindingByRef(ctx context.Context, user, group string) error
}
