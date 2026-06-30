package mocks

import (
	"context"
	"io"

	"github.com/okdp/okdp-server-new/internal/models"
	"github.com/okdp/okdp-server-new/internal/repository/crd"
	"github.com/stretchr/testify/mock"
	"k8s.io/apimachinery/pkg/watch"
)

// ProjectRepository Mock
type ProjectRepository struct {
	mock.Mock
}

func (m *ProjectRepository) List(ctx context.Context) ([]models.Project, error) {
	args := m.Called(ctx)
	return args.Get(0).([]models.Project), args.Error(1)
}

func (m *ProjectRepository) Get(ctx context.Context, name string) (*models.Project, error) {
	args := m.Called(ctx, name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.Project), args.Error(1)
}

func (m *ProjectRepository) Create(ctx context.Context, project *models.Project) error {
	args := m.Called(ctx, project)
	return args.Error(0)
}

func (m *ProjectRepository) Delete(ctx context.Context, name string) error {
	args := m.Called(ctx, name)
	return args.Error(0)
}

func (m *ProjectRepository) Watch(ctx context.Context) (watch.Interface, error) {
	args := m.Called(ctx)
	return args.Get(0).(watch.Interface), args.Error(1)
}

// ContextWriterRepository Mock
type ContextWriterRepository struct {
	mock.Mock
}

func (m *ContextWriterRepository) CreateFromDefault(ctx context.Context, projectName string) error {
	args := m.Called(ctx, projectName)
	return args.Error(0)
}

func (m *ContextWriterRepository) SyncFromDefault(ctx context.Context, projectName string) error {
	args := m.Called(ctx, projectName)
	return args.Error(0)
}

func (m *ContextWriterRepository) Delete(ctx context.Context, projectName string) error {
	args := m.Called(ctx, projectName)
	return args.Error(0)
}

func (m *ContextWriterRepository) AddPlatformService(ctx context.Context, svc models.PlatformService) error {
	args := m.Called(ctx, svc)
	return args.Error(0)
}

func (m *ContextWriterRepository) UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) error {
	args := m.Called(ctx, name, svc)
	return args.Error(0)
}

func (m *ContextWriterRepository) RemovePlatformService(ctx context.Context, name string) error {
	args := m.Called(ctx, name)
	return args.Error(0)
}

// SecretStoreRepository Mock
type SecretStoreRepository struct {
	mock.Mock
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

func (m *IdentityRepository) CreateUser(ctx context.Context, user *crd.User) error {
	args := m.Called(ctx, user)
	return args.Error(0)
}

func (m *IdentityRepository) UpdateUser(ctx context.Context, user *crd.User) error {
	args := m.Called(ctx, user)
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

func (m *IdentityRepository) CreateGroup(ctx context.Context, group *crd.Group) error {
	args := m.Called(ctx, group)
	return args.Error(0)
}

func (m *IdentityRepository) UpdateGroup(ctx context.Context, group *crd.Group) error {
	args := m.Called(ctx, group)
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

func (m *IdentityRepository) CreateGroupBinding(ctx context.Context, binding *crd.GroupBinding) error {
	args := m.Called(ctx, binding)
	return args.Error(0)
}

func (m *IdentityRepository) DeleteGroupBinding(ctx context.Context, name string) error {
	args := m.Called(ctx, name)
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

// ServiceService Mock
type ServiceService struct {
	mock.Mock
}

func (m *ServiceService) GetPlatformServices(ctx context.Context) ([]models.PlatformService, error) {
	args := m.Called(ctx)
	return args.Get(0).([]models.PlatformService), args.Error(1)
}

func (m *ServiceService) AddPlatformService(ctx context.Context, svc models.PlatformService) (*models.PlatformService, error) {
	args := m.Called(ctx, svc)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.PlatformService), args.Error(1)
}

func (m *ServiceService) UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) (*models.PlatformService, error) {
	args := m.Called(ctx, name, svc)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.PlatformService), args.Error(1)
}

func (m *ServiceService) RemovePlatformService(ctx context.Context, name string) error {
	args := m.Called(ctx, name)
	return args.Error(0)
}

func (m *ServiceService) DeployService(ctx context.Context, project string, req models.ServiceRequest) (*models.ServiceInstance, error) {
	args := m.Called(ctx, project, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.ServiceInstance), args.Error(1)
}

func (m *ServiceService) ListServices(ctx context.Context, project string) ([]models.ServiceInstance, error) {
	args := m.Called(ctx, project)
	return args.Get(0).([]models.ServiceInstance), args.Error(1)
}

func (m *ServiceService) GetService(ctx context.Context, project, name string) (*models.ServiceInstance, error) {
	args := m.Called(ctx, project, name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.ServiceInstance), args.Error(1)
}

func (m *ServiceService) UpdateServiceParameters(ctx context.Context, project, name string, params map[string]any) (*models.ServiceInstance, error) {
	args := m.Called(ctx, project, name, params)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.ServiceInstance), args.Error(1)
}

func (m *ServiceService) DeleteService(ctx context.Context, project, name string) error {
	args := m.Called(ctx, project, name)
	return args.Error(0)
}

func (m *ServiceService) WatchServices(ctx context.Context, project string) (watch.Interface, error) {
	args := m.Called(ctx, project)
	return args.Get(0).(watch.Interface), args.Error(1)
}

func (m *ServiceService) GetCatalog(ctx context.Context) ([]models.CatalogCategory, error) {
	args := m.Called(ctx)
	return args.Get(0).([]models.CatalogCategory), args.Error(1)
}

func (m *ServiceService) GetIngressSuffix(ctx context.Context) (string, error) {
	args := m.Called(ctx)
	return args.String(0), args.Error(1)
}

func (m *ServiceService) GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error) {
	args := m.Called(ctx)
	return args.Get(0).(map[string][]models.ProfileImage), args.Error(1)
}

func (m *ServiceService) EnrichPodHealth(ctx context.Context, instance *models.ServiceInstance) {
	m.Called(ctx, instance)
}

func (m *ServiceService) ListPods(ctx context.Context, project, serviceName string) ([]models.Pod, error) {
	args := m.Called(ctx, project, serviceName)
	return args.Get(0).([]models.Pod), args.Error(1)
}

func (m *ServiceService) GetServiceMetrics(ctx context.Context, project, serviceName string) (*models.ServiceMetrics, error) {
	args := m.Called(ctx, project, serviceName)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.ServiceMetrics), args.Error(1)
}

func (m *ServiceService) GetPodLogs(ctx context.Context, project, podName, container string, tailLines int64, follow bool) (io.ReadCloser, error) {
	args := m.Called(ctx, project, podName, container, tailLines, follow)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(io.ReadCloser), args.Error(1)
}
