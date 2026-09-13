package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var projectsResource = schema.GroupResource{Group: "okdp.io", Resource: "projects"}

// ProjectService defines the business logic for projects
type ProjectService interface {
	ListProjects(ctx context.Context) ([]models.Project, error)
	GetProject(ctx context.Context, name string) (*models.Project, error)
	CreateProject(ctx context.Context, project *models.Project) error
	UpdateProject(ctx context.Context, project *models.Project) (*models.Project, error)
	DeleteProject(ctx context.Context, name string) error
	// WatchProjects streams the projects as they change, starting with an
	// ADDED event per existing project, until ctx ends.
	WatchProjects(ctx context.Context) (<-chan ProjectEvent, error)
}

// ProjectEvent is one change of the projects, as streamed to the console:
// Type is ADDED, MODIFIED or DELETED.
type ProjectEvent struct {
	Type   string         `json:"type"`
	Object models.Project `json:"object"`
}

// DefaultProjectPollInterval is how often a project stream reads the
// deployments repository for projects written outside the console.
const DefaultProjectPollInterval = 5 * time.Second

// DefaultProjectService serves the projects declared in the deployments
// repository (projects/<p>/project.yaml): a project written by hand in Git
// and one created from the console are the same. The project Namespace is
// runtime state, managed on the side (see repository.ProjectNamespaceRepository).
type DefaultProjectService struct {
	namespaces  repository.ProjectNamespaceRepository
	deployments *gitops.Deployments
	// PollInterval is how often WatchProjects reads Git; the console's own
	// changes are streamed at once.
	PollInterval time.Duration

	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

// NewDefaultProjectService creates a new DefaultProjectService.
func NewDefaultProjectService(namespaces repository.ProjectNamespaceRepository, deployments *gitops.Deployments) *DefaultProjectService {
	return &DefaultProjectService{
		namespaces:   namespaces,
		deployments:  deployments,
		PollInterval: DefaultProjectPollInterval,
		subs:         map[chan struct{}]struct{}{},
	}
}

// projectError maps the errors of the deployments repository to the API
// errors the handlers and middleware.RequireProject understand.
func projectError(err error, name string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gitops.ErrNotFound):
		return apierrors.NewNotFound(projectsResource, name)
	case errors.Is(err, gitops.ErrExists):
		return apierrors.NewAlreadyExists(projectsResource, name)
	default:
		return err
	}
}

func toModel(p gitops.Project) models.Project {
	return models.Project{Name: p.Name, Description: p.Description}
}

// ListProjects returns the projects declared in Git, sorted by name.
func (s *DefaultProjectService) ListProjects(ctx context.Context) ([]models.Project, error) {
	projects, err := s.deployments.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]models.Project, 0, len(projects))
	for _, p := range projects {
		out = append(out, toModel(p))
	}
	return out, nil
}

// GetProject returns a project declared in Git, or a NotFound error.
func (s *DefaultProjectService) GetProject(ctx context.Context, name string) (*models.Project, error) {
	p, err := s.deployments.GetProject(ctx, name)
	if err != nil {
		return nil, projectError(err, name)
	}
	project := toModel(*p)
	return &project, nil
}

// CreateProject creates its Namespace, then commits projects/<p>/project.yaml.
// The Namespace goes first because it refuses a name that is already a
// Namespace other than a project's (kube-system, …): a project named after
// it would hand that namespace to the console. It is removed again when the
// commit fails.
func (s *DefaultProjectService) CreateProject(ctx context.Context, project *models.Project) error {
	if err := gitops.ValidateName("project", project.Name); err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	created := false
	if s.namespaces != nil {
		var err error
		if created, err = s.namespaces.CreateNamespace(ctx, project); err != nil {
			return err
		}
	}
	_, err := s.deployments.CreateProject(ctx, auth.ActorFrom(ctx), gitops.Project{Name: project.Name, Description: project.Description})
	if err != nil {
		if created {
			if _, rollbackErr := s.namespaces.DeleteNamespace(ctx, project.Name); rollbackErr != nil {
				logrus.WithError(rollbackErr).WithField("project", project.Name).Warn("Could not remove the namespace of a project Git refused")
			}
		}
		return projectError(err, project.Name)
	}
	s.notify()
	return nil
}

// UpdateProject changes the description in project.yaml.
func (s *DefaultProjectService) UpdateProject(ctx context.Context, project *models.Project) (*models.Project, error) {
	if _, err := s.deployments.UpdateProject(ctx, auth.ActorFrom(ctx), gitops.Project{Name: project.Name, Description: project.Description}); err != nil {
		return nil, projectError(err, project.Name)
	}
	s.notify()
	return &models.Project{Name: project.Name, Description: project.Description}, nil
}

// DeleteProject removes projects/<p>/ from Git, so the GitOps engine
// uninstalls its releases, then the Namespace when the console created it.
// A Namespace the engine created is left, as it would be if the directory
// were removed by hand.
func (s *DefaultProjectService) DeleteProject(ctx context.Context, name string) error {
	if _, err := s.deployments.DeleteProject(ctx, auth.ActorFrom(ctx), name); err != nil {
		return projectError(err, name)
	}
	s.notify()
	if s.namespaces == nil {
		return nil
	}
	if _, err := s.namespaces.DeleteNamespace(ctx, name); err != nil {
		// The project is gone from Git, which is what the answer reports.
		logrus.WithError(err).WithField("project", name).Warn("Could not delete the namespace of a deleted project")
	}
	return nil
}

// notify wakes the project streams after one of the console's own commits.
func (s *DefaultProjectService) notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *DefaultProjectService) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// WatchProjects reads the projects from Git every PollInterval, and at once
// after the console's own changes, and streams the differences. A read that
// fails is retried on the next tick.
func (s *DefaultProjectService) WatchProjects(ctx context.Context) (<-chan ProjectEvent, error) {
	current, err := s.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	interval := s.PollInterval
	if interval <= 0 {
		interval = DefaultProjectPollInterval
	}
	changes, unsubscribe := s.subscribe()
	out := make(chan ProjectEvent, 16)

	go func() {
		defer close(out)
		defer unsubscribe()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		known := map[string]models.Project{}
		send := func(eventType string, p models.Project) bool {
			select {
			case out <- ProjectEvent{Type: eventType, Object: p}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for _, p := range current {
			known[p.Name] = p
			if !send("ADDED", p) {
				return
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-changes:
			}
			projects, err := s.ListProjects(ctx)
			if err != nil {
				logrus.WithError(err).Warn("Could not read the projects for a project stream")
				continue
			}
			seen := map[string]bool{}
			for _, p := range projects {
				seen[p.Name] = true
				previous, existed := known[p.Name]
				known[p.Name] = p
				switch {
				case !existed:
					if !send("ADDED", p) {
						return
					}
				case previous != p:
					if !send("MODIFIED", p) {
						return
					}
				}
			}
			for name, p := range known {
				if !seen[name] {
					delete(known, name)
					if !send("DELETED", p) {
						return
					}
				}
			}
		}
	}()
	return out, nil
}
