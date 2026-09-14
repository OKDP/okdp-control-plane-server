package mocks

import (
	"context"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/stretchr/testify/mock"
)

// ProjectNamespaceRepository Mock
type ProjectNamespaceRepository struct {
	mock.Mock
}

func (m *ProjectNamespaceRepository) CreateNamespace(ctx context.Context, project *models.Project) (bool, error) {
	args := m.Called(ctx, project)
	return args.Bool(0), args.Error(1)
}

func (m *ProjectNamespaceRepository) DeleteNamespace(ctx context.Context, name string) (bool, error) {
	args := m.Called(ctx, name)
	return args.Bool(0), args.Error(1)
}

// SecretStoreRepository Mock
type SecretStoreRepository struct {
	mock.Mock
}

// Available defaults to true so a test that only exercises store logic does not
// have to arrange the CRD-presence check first.
func (m *SecretStoreRepository) Available(ctx context.Context) bool {
	if len(m.ExpectedCalls) == 0 {
		return true
	}
	for _, call := range m.ExpectedCalls {
		if call.Method == "Available" {
			return m.Called(ctx).Bool(0)
		}
	}
	return true
}

func (m *SecretStoreRepository) Create(ctx context.Context, namespace string, store *crd.ESOSecretStore) error {
	args := m.Called(ctx, namespace, store)
	return args.Error(0)
}

func (m *SecretStoreRepository) Get(ctx context.Context, namespace, name string) (*crd.ESOSecretStore, error) {
	args := m.Called(ctx, namespace, name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*crd.ESOSecretStore), args.Error(1)
}

func (m *SecretStoreRepository) List(ctx context.Context, namespace string) ([]crd.ESOSecretStore, error) {
	args := m.Called(ctx, namespace)
	return args.Get(0).([]crd.ESOSecretStore), args.Error(1)
}

func (m *SecretStoreRepository) Update(ctx context.Context, namespace string, store *crd.ESOSecretStore) error {
	args := m.Called(ctx, namespace, store)
	return args.Error(0)
}

func (m *SecretStoreRepository) Delete(ctx context.Context, namespace, name string) error {
	args := m.Called(ctx, namespace, name)
	return args.Error(0)
}

func (m *SecretStoreRepository) CreateOrUpdateSecret(ctx context.Context, namespace, name string, data map[string][]byte) error {
	args := m.Called(ctx, namespace, name, data)
	return args.Error(0)
}

func (m *SecretStoreRepository) GetSecretData(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	args := m.Called(ctx, namespace, name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(map[string][]byte), args.Error(1)
}

func (m *SecretStoreRepository) DeleteSecret(ctx context.Context, namespace, name string) error {
	args := m.Called(ctx, namespace, name)
	return args.Error(0)
}

func (m *SecretStoreRepository) RemoveDefaultLabel(ctx context.Context, namespace string) error {
	args := m.Called(ctx, namespace)
	return args.Error(0)
}

// IdentityRepository Mock
type IdentityRepository struct {
	mock.Mock
}

func (m *IdentityRepository) Available(ctx context.Context) bool {
	args := m.Called(ctx)
	return args.Bool(0)
}

func (m *IdentityRepository) ListUsers(ctx context.Context) ([]models.User, error) {
	args := m.Called(ctx)
	return args.Get(0).([]models.User), args.Error(1)
}

func (m *IdentityRepository) GetUser(ctx context.Context, name string) (*models.User, error) {
	args := m.Called(ctx, name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.User), args.Error(1)
}

func (m *IdentityRepository) CreateUser(ctx context.Context, user *models.User) error {
	args := m.Called(ctx, user)
	return args.Error(0)
}

func (m *IdentityRepository) UpdateUser(ctx context.Context, name string, user *models.User) error {
	args := m.Called(ctx, name, user)
	return args.Error(0)
}

func (m *IdentityRepository) DeleteUser(ctx context.Context, name string) error {
	args := m.Called(ctx, name)
	return args.Error(0)
}

func (m *IdentityRepository) ListGroups(ctx context.Context) ([]models.Group, error) {
	args := m.Called(ctx)
	return args.Get(0).([]models.Group), args.Error(1)
}

func (m *IdentityRepository) GetGroup(ctx context.Context, name string) (*models.Group, error) {
	args := m.Called(ctx, name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.Group), args.Error(1)
}

func (m *IdentityRepository) CreateGroup(ctx context.Context, group *models.Group) error {
	args := m.Called(ctx, group)
	return args.Error(0)
}

func (m *IdentityRepository) UpdateGroup(ctx context.Context, name string, group *models.Group) error {
	args := m.Called(ctx, name, group)
	return args.Error(0)
}

func (m *IdentityRepository) DeleteGroup(ctx context.Context, name string) error {
	args := m.Called(ctx, name)
	return args.Error(0)
}

func (m *IdentityRepository) ListGroupBindings(ctx context.Context, userFilter string) ([]models.GroupBinding, error) {
	args := m.Called(ctx, userFilter)
	return args.Get(0).([]models.GroupBinding), args.Error(1)
}

func (m *IdentityRepository) CreateGroupBinding(ctx context.Context, user, group string) error {
	args := m.Called(ctx, user, group)
	return args.Error(0)
}

func (m *IdentityRepository) DeleteGroupBindingByRef(ctx context.Context, user, group string) error {
	args := m.Called(ctx, user, group)
	return args.Error(0)
}

// ExternalSecretRepository Mock
type ExternalSecretRepository struct {
	mock.Mock
}

func (m *ExternalSecretRepository) Create(ctx context.Context, namespace string, es *crd.ESOExternalSecret) error {
	args := m.Called(ctx, namespace, es)
	return args.Error(0)
}

func (m *ExternalSecretRepository) Get(ctx context.Context, namespace, name string) (*crd.ESOExternalSecret, error) {
	args := m.Called(ctx, namespace, name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*crd.ESOExternalSecret), args.Error(1)
}

func (m *ExternalSecretRepository) List(ctx context.Context, namespace string) ([]crd.ESOExternalSecret, error) {
	args := m.Called(ctx, namespace)
	return args.Get(0).([]crd.ESOExternalSecret), args.Error(1)
}

func (m *ExternalSecretRepository) Update(ctx context.Context, namespace string, es *crd.ESOExternalSecret) error {
	args := m.Called(ctx, namespace, es)
	return args.Error(0)
}

func (m *ExternalSecretRepository) Delete(ctx context.Context, namespace, name string) error {
	args := m.Called(ctx, namespace, name)
	return args.Error(0)
}

// ConnectionSecretRepository Mock (credentials Secrets of external connections)
type ConnectionSecretRepository struct {
	mock.Mock
}

func (m *ConnectionSecretRepository) CreateOrUpdateSecret(ctx context.Context, namespace, name string, data map[string][]byte) error {
	args := m.Called(ctx, namespace, name, data)
	return args.Error(0)
}

func (m *ConnectionSecretRepository) DeleteSecret(ctx context.Context, namespace, name string) error {
	args := m.Called(ctx, namespace, name)
	return args.Error(0)
}

func (m *ConnectionSecretRepository) InspectSecret(ctx context.Context, namespace, name string) (repository.SecretContent, bool, error) {
	args := m.Called(ctx, namespace, name)
	var content repository.SecretContent
	if raw := args.Get(0); raw != nil {
		content = raw.(repository.SecretContent)
	}
	return content, args.Bool(1), args.Error(2)
}
