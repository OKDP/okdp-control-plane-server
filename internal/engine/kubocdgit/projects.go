package kubocdgit

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"

	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

var namespaceGR = schema.GroupResource{Resource: "namespaces"}

// ProjectRepository stores each project as projects/<name>/namespace.yaml,
// labelled okdp.io/project: <name>. Any non-empty value of the label marks a
// project.
// It reads git first and the cluster after: a project committed and not
// applied yet is listed, and so is one deleted from git that the sync tool
// has not pruned yet. A project that exists only in the cluster is listed
// but cannot be changed from here.
type ProjectRepository struct {
	adapter *Adapter
	client  kubernetes.Interface
}

var _ repository.ProjectRepository = (*ProjectRepository)(nil)

// NewProjectRepository returns the git-backed project repository.
func NewProjectRepository(adapter *Adapter, client kubernetes.Interface) *ProjectRepository {
	return &ProjectRepository{adapter: adapter, client: client}
}

func namespaceManifest(project *models.Project) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name":        project.Name,
			"labels":      map[string]any{repository.ProjectLabel: project.Name},
			"annotations": map[string]any{repository.ProjectDescriptionAnnot: project.Description},
		},
	}
}

const maxProjectDescriptionBytes = 4096

func validateProjectDescription(description string) error {
	if len(description) > maxProjectDescriptionBytes {
		return apierrors.NewBadRequest(fmt.Sprintf("project description must be at most %d bytes", maxProjectDescriptionBytes))
	}
	return nil
}

// Create commits the Namespace. A namespace that already exists in the
// cluster, a project or not, is refused like the API server refuses it.
func (r *ProjectRepository) Create(ctx context.Context, project *models.Project) error {
	if err := validateProjectDescription(project.Description); err != nil {
		return err
	}
	p, err := gitstore.NamespaceFile(project.Name)
	if err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if _, err := r.client.CoreV1().Namespaces().Get(ctx, project.Name, metav1.GetOptions{}); err == nil {
		return apierrors.NewAlreadyExists(namespaceGR, project.Name)
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	content, err := render(namespaceManifest(project))
	if err != nil {
		return err
	}
	_, err = r.adapter.commit(ctx, gitstore.WriteRequest{
		Changes: []gitstore.Change{{Path: p, Content: content, MustNotExist: true}},
		Message: "Create project " + project.Name,
	})
	return storeError(err, namespaceGR, project.Name)
}

// storeError turns a store error into the Kubernetes error the handlers
// already map to a status code.
func storeError(err error, gr schema.GroupResource, name string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gitstore.ErrAlreadyExists):
		return apierrors.NewAlreadyExists(gr, name)
	case errors.Is(err, gitstore.ErrNotFound):
		return apierrors.NewNotFound(gr, name)
	case errors.Is(err, gitstore.ErrConflict):
		return apierrors.NewConflict(gr, name, err)
	case errors.Is(err, gitstore.ErrInvalidPath), errors.Is(err, gitstore.ErrInvalidRequest):
		return apierrors.NewBadRequest(err.Error())
	}
	return err
}

// gitProject reads a project from snap, nil when git does not hold it.
func gitProject(snap *gitstore.Snapshot, name string) (*models.Project, []byte, error) {
	p, err := gitstore.NamespaceFile(name)
	if err != nil {
		return nil, nil, nil
	}
	data, err := snap.ReadFile(p)
	if errors.Is(err, gitstore.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	obj, err := decode(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", p, err)
	}
	u := unstructured.Unstructured{Object: obj}
	if u.GetKind() != "Namespace" || u.GetName() != name {
		return nil, nil, fmt.Errorf("%s is not Namespace %q (got kind=%q name=%q)", p, name, u.GetKind(), u.GetName())
	}
	if u.GetLabels()[repository.ProjectLabel] == "" {
		return nil, nil, nil
	}
	return &models.Project{Name: name, Description: u.GetAnnotations()[repository.ProjectDescriptionAnnot]}, data, nil
}

func (r *ProjectRepository) clusterProject(ctx context.Context, name string) (*models.Project, error) {
	ns, err := r.client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if ns.Labels[repository.ProjectLabel] == "" {
		return nil, apierrors.NewNotFound(namespaceGR, name)
	}
	return &models.Project{Name: ns.Name, Description: ns.Annotations[repository.ProjectDescriptionAnnot]}, nil
}

func (r *ProjectRepository) Get(ctx context.Context, name string) (*models.Project, error) {
	snap, err := r.adapter.snapshots.current(ctx)
	if err != nil {
		return nil, err
	}
	project, _, err := gitProject(snap, name)
	if err != nil || project != nil {
		return project, err
	}
	if refreshed, rerr := r.adapter.snapshots.refresh(ctx); rerr == nil && refreshed.Revision() != snap.Revision() {
		project, _, err = gitProject(refreshed, name)
		if err != nil || project != nil {
			return project, err
		}
	}
	return r.clusterProject(ctx, name)
}

func (r *ProjectRepository) List(ctx context.Context) ([]models.Project, error) {
	snap, err := r.adapter.snapshots.current(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]models.Project{}
	entries, err := snap.ReadDir("projects")
	if err != nil && !errors.Is(err, gitstore.ErrNotFound) {
		return nil, err
	}
	for _, e := range entries {
		if e.Kind != gitstore.KindDir {
			continue
		}
		project, _, err := gitProject(snap, e.Name)
		if err != nil {
			logrus.WithError(err).WithField("project", e.Name).Warn("Skipping a project directory with an invalid namespace manifest")
			continue
		}
		if project != nil {
			byName[project.Name] = *project
		}
	}
	list, err := r.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: repository.ProjectLabel})
	if err != nil {
		return nil, err
	}
	for _, ns := range list.Items {
		if _, ok := byName[ns.Name]; !ok {
			byName[ns.Name] = models.Project{Name: ns.Name, Description: ns.Annotations[repository.ProjectDescriptionAnnot]}
		}
	}
	projects := make([]models.Project, 0, len(byName))
	for _, p := range byName {
		projects = append(projects, p)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects, nil
}

// notInGit refuses a change to a project the cluster holds and git does not.
func (r *ProjectRepository) notInGit(ctx context.Context, name string) error {
	if _, err := r.clusterProject(ctx, name); err != nil {
		return err
	}
	return apierrors.NewConflict(namespaceGR, name, errors.New("the project exists in the cluster but not in git, change it in git or in the cluster"))
}

// Update sets the description, keeping every other field of the file.
func (r *ProjectRepository) Update(ctx context.Context, project *models.Project) (*models.Project, error) {
	if err := validateProjectDescription(project.Description); err != nil {
		return nil, err
	}
	p, err := gitstore.NamespaceFile(project.Name)
	if err != nil {
		return nil, apierrors.NewNotFound(namespaceGR, project.Name)
	}
	for attempt := 0; ; attempt++ {
		snap, err := r.adapter.snapshots.latest(ctx)
		if err != nil {
			return nil, err
		}
		current, data, err := gitProject(snap, project.Name)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, r.notInGit(ctx, project.Name)
		}
		rev, err := r.adapter.snapshots.fileRevision(snap, p)
		if err != nil {
			return nil, err
		}
		doc, err := parseDocument(data)
		if err != nil {
			return nil, err
		}
		if err := doc.set(project.Description, "metadata", "annotations", repository.ProjectDescriptionAnnot); err != nil {
			return nil, err
		}
		content, err := doc.bytes()
		if err != nil {
			return nil, err
		}
		_, err = r.adapter.commit(ctx, gitstore.WriteRequest{
			Changes: []gitstore.Change{{Path: p, Content: content, ExpectedRevision: rev}},
			Message: "Update project " + project.Name,
		})
		if errors.Is(err, gitstore.ErrConflict) && attempt < maxCASAttempts {
			continue
		}
		if err != nil {
			return nil, storeError(err, namespaceGR, project.Name)
		}
		return &models.Project{Name: project.Name, Description: project.Description}, nil
	}
}

// Delete removes projects/<name>/ with everything it holds (Namespace,
// Contexts, Releases, Connections) in one commit.
func (r *ProjectRepository) Delete(ctx context.Context, name string) error {
	dir, err := gitstore.ProjectDir(name)
	if err != nil {
		return apierrors.NewNotFound(namespaceGR, name)
	}
	snap, err := r.adapter.snapshots.latest(ctx)
	if err != nil {
		return err
	}
	project, _, err := gitProject(snap, name)
	if err != nil {
		return err
	}
	if project == nil {
		return r.notInGit(ctx, name)
	}
	_, err = r.adapter.commit(ctx, gitstore.WriteRequest{
		Changes: []gitstore.Change{{Path: dir, DeleteTree: true}},
		Message: "Delete project " + name,
	})
	return storeError(err, namespaceGR, name)
}

// Watch follows the project Namespaces of the cluster, as the sync tool
// applies them.
func (r *ProjectRepository) Watch(ctx context.Context) (watch.Interface, error) {
	return r.client.CoreV1().Namespaces().Watch(ctx, metav1.ListOptions{LabelSelector: repository.ProjectLabel})
}
