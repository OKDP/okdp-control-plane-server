package service

import (
	"context"
	"fmt"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/watch"
)

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
	repo        repository.ProjectRepository
	deployments *gitops.Deployments
}

// NewDefaultProjectService creates a new DefaultProjectService. deployments
// may be nil, in which case projects exist in the cluster only.
func NewDefaultProjectService(repo repository.ProjectRepository, deployments *gitops.Deployments) *DefaultProjectService {
	return &DefaultProjectService{repo: repo, deployments: deployments}
}

// ListProjects returns all projects
func (s *DefaultProjectService) ListProjects(ctx context.Context) ([]models.Project, error) {
	return s.repo.List(ctx)
}

// GetProject returns a single project
func (s *DefaultProjectService) GetProject(ctx context.Context, name string) (*models.Project, error) {
	return s.repo.Get(ctx, name)
}

// CreateProject creates a new project: its Namespace, which the charts
// deploy into, and its projects/<p>/project.yaml in the deployments
// repository. The Namespace goes first because it is what answers "already
// exists"; it is removed again when the commit fails.
func (s *DefaultProjectService) CreateProject(ctx context.Context, project *models.Project) error {
	if err := s.repo.Create(ctx, project); err != nil {
		return err
	}
	if s.deployments == nil {
		return nil
	}
	if _, err := s.deployments.PutProject(ctx, auth.ActorName(ctx), gitops.Project{Name: project.Name, Description: project.Description}); err != nil {
		if rollbackErr := s.repo.Delete(ctx, project.Name); rollbackErr != nil {
			logrus.WithError(rollbackErr).WithField("project", project.Name).Warn("Could not remove the namespace of a project Git refused")
		}
		return fmt.Errorf("failed to declare the project in the deployments repository: %w", err)
	}
	return nil
}

// UpdateProject updates a project's mutable metadata (its description) on the
// backing Namespace and in project.yaml.
func (s *DefaultProjectService) UpdateProject(ctx context.Context, project *models.Project) (*models.Project, error) {
	updated, err := s.repo.Update(ctx, project)
	if err != nil || s.deployments == nil {
		return updated, err
	}
	if _, err := s.deployments.PutProject(ctx, auth.ActorName(ctx), gitops.Project{Name: updated.Name, Description: updated.Description}); err != nil {
		return nil, fmt.Errorf("failed to update the project in the deployments repository: %w", err)
	}
	return updated, nil
}

// DeleteProject deletes a project: its declarations in Git first, so the
// GitOps engine uninstalls its releases, then its Namespace.
func (s *DefaultProjectService) DeleteProject(ctx context.Context, name string) error {
	if s.deployments != nil {
		if _, err := s.deployments.DeleteProject(ctx, auth.ActorName(ctx), name); err != nil {
			return fmt.Errorf("failed to remove the project from the deployments repository: %w", err)
		}
	}
	return s.repo.Delete(ctx, name)
}

// WatchProjects watches for project changes
func (s *DefaultProjectService) WatchProjects(ctx context.Context) (watch.Interface, error) {
	return s.repo.Watch(ctx)
}
