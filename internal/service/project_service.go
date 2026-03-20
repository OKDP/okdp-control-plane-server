package service

import (
	"context"

	"github.com/okdp/okdp-server-new/internal/models"
	"github.com/okdp/okdp-server-new/internal/repository"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/watch"
)

// ProjectService defines the business logic for projects
type ProjectService interface {
	ListProjects(ctx context.Context) ([]models.Project, error)
	GetProject(ctx context.Context, name string) (*models.Project, error)
	CreateProject(ctx context.Context, project *models.Project) error
	DeleteProject(ctx context.Context, name string) error
	WatchProjects(ctx context.Context) (watch.Interface, error)
}

// DefaultProjectService is the default implementation of ProjectService
type DefaultProjectService struct {
	repo             repository.ProjectRepository
	contextWriteRepo repository.ContextWriterRepository
}

// NewDefaultProjectService creates a new DefaultProjectService
func NewDefaultProjectService(repo repository.ProjectRepository, contextWriteRepo repository.ContextWriterRepository) *DefaultProjectService {
	return &DefaultProjectService{
		repo:             repo,
		contextWriteRepo: contextWriteRepo,
	}
}

// ListProjects returns all projects
func (s *DefaultProjectService) ListProjects(ctx context.Context) ([]models.Project, error) {
	return s.repo.List(ctx)
}

// GetProject returns a single project
func (s *DefaultProjectService) GetProject(ctx context.Context, name string) (*models.Project, error) {
	return s.repo.Get(ctx, name)
}

// CreateProject creates a new project (backed by a Kubernetes Namespace) and
// provisions its per-project Context CR cloned from the default one.
func (s *DefaultProjectService) CreateProject(ctx context.Context, project *models.Project) error {
	if err := s.repo.Create(ctx, project); err != nil {
		return err
	}

	if s.contextWriteRepo != nil {
		if err := s.contextWriteRepo.CreateFromDefault(ctx, project.Name); err != nil {
			logrus.WithError(err).WithField("project", project.Name).Warn("Failed to create per-project Context (project was created)")
		}
	}

	return nil
}

// DeleteProject deletes a project (its Namespace) and its per-project Context CR.
func (s *DefaultProjectService) DeleteProject(ctx context.Context, name string) error {
	if s.contextWriteRepo != nil {
		if err := s.contextWriteRepo.Delete(ctx, name); err != nil {
			logrus.WithError(err).WithField("project", name).Warn("Failed to delete per-project Context")
		}
	}

	return s.repo.Delete(ctx, name)
}

// WatchProjects watches for project changes
func (s *DefaultProjectService) WatchProjects(ctx context.Context) (watch.Interface, error) {
	return s.repo.Watch(ctx)
}
