package service

import (
	"context"
	"fmt"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/watch"
)

// maxProjectDescriptionBytes bounds the okdp.io/description annotation written
// to Kubernetes Namespaces and Git manifests.
const maxProjectDescriptionBytes = 4096

func validateProject(project *models.Project) error {
	if project == nil {
		return apierrors.NewBadRequest("project is required")
	}
	if len(project.Description) > maxProjectDescriptionBytes {
		return apierrors.NewBadRequest(fmt.Sprintf("project description must be at most %d bytes", maxProjectDescriptionBytes))
	}
	return nil
}

// ProjectService defines the business logic for projects
type ProjectService interface {
	ListProjects(ctx context.Context) ([]models.Project, error)
	GetProject(ctx context.Context, name string) (*models.Project, error)
	CreateProject(ctx context.Context, project *models.Project) error
	UpdateProject(ctx context.Context, project *models.Project) (*models.Project, error)
	DeleteProject(ctx context.Context, name string) error
	WatchProjects(ctx context.Context) (watch.Interface, error)
}

// DefaultProjectService is the default implementation of ProjectService
type DefaultProjectService struct {
	repo repository.ProjectRepository
}

// NewDefaultProjectService creates a new DefaultProjectService
func NewDefaultProjectService(repo repository.ProjectRepository) *DefaultProjectService {
	return &DefaultProjectService{repo: repo}
}

// ListProjects returns all projects
func (s *DefaultProjectService) ListProjects(ctx context.Context) ([]models.Project, error) {
	return s.repo.List(ctx)
}

// GetProject returns a single project
func (s *DefaultProjectService) GetProject(ctx context.Context, name string) (*models.Project, error) {
	return s.repo.Get(ctx, name)
}

// CreateProject creates a new project, backed by a Kubernetes Namespace.
//
// No Context is created for the project. KuboCD resolves an optional Context by
// name in the namespace of each Release, through Config.defaultNamespaceContexts,
// so a project that overrides nothing needs no object at all. A project that
// does override something declares its own Context, and it survives.
func (s *DefaultProjectService) CreateProject(ctx context.Context, project *models.Project) error {
	if err := validateProject(project); err != nil {
		return err
	}
	return s.repo.Create(ctx, project)
}

// UpdateProject updates a project's mutable metadata (its description) on the
// backing Namespace.
func (s *DefaultProjectService) UpdateProject(ctx context.Context, project *models.Project) (*models.Project, error) {
	if err := validateProject(project); err != nil {
		return nil, err
	}
	return s.repo.Update(ctx, project)
}

// DeleteProject deletes a project, that is its Namespace. A Context the project
// may have declared lives in that namespace and goes with it.
func (s *DefaultProjectService) DeleteProject(ctx context.Context, name string) error {
	return s.repo.Delete(ctx, name)
}

// WatchProjects watches for project changes
func (s *DefaultProjectService) WatchProjects(ctx context.Context) (watch.Interface, error) {
	return s.repo.Watch(ctx)
}
