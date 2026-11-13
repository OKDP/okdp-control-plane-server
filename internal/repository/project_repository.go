package repository

import (
	"context"

	"github.com/sirupsen/logrus"

	"github.com/okdp/okdp-control-plane-server/internal/models"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

var namespaceGR = schema.GroupResource{Group: "", Resource: "namespaces"}

const (
	ProjectLabel            = "okdp.io/project"
	ProjectDescriptionAnnot = "okdp.io/description"
)

type ProjectRepository interface {
	Create(ctx context.Context, project *models.Project) error
	Get(ctx context.Context, name string) (*models.Project, error)
	List(ctx context.Context) ([]models.Project, error)
	Update(ctx context.Context, project *models.Project) (*models.Project, error)
	Delete(ctx context.Context, name string) error
	Watch(ctx context.Context) (watch.Interface, error)
}

type k8sProjectRepository struct {
	client   kubernetes.Interface
	platform kubernetes.Interface
}

// NewProjectRepository creates a project repository backed by Kubernetes Namespaces.
// A project is materialized as a Namespace carrying the label okdp.io/project=<name>
// and the annotation okdp.io/description=<description>.
//
// client acts as the caller. platform acts as the server and is only used to
// list and watch: a caller with access to some projects cannot list
// namespaces, so the server lists them and keeps those the caller can read.
func NewProjectRepository(client, platform kubernetes.Interface) ProjectRepository {
	return &k8sProjectRepository{client: client, platform: platform}
}

func (r *k8sProjectRepository) Create(ctx context.Context, project *models.Project) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: project.Name,
			Labels: map[string]string{
				ProjectLabel: project.Name,
			},
			Annotations: map[string]string{
				ProjectDescriptionAnnot: project.Description,
			},
		},
	}

	_, err := r.client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	return err
}

func (r *k8sProjectRepository) Get(ctx context.Context, name string) (*models.Project, error) {
	ns, err := r.client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	if ns.Labels[ProjectLabel] == "" {
		return nil, apierrors.NewNotFound(namespaceGR, name)
	}

	return namespaceToProject(ns), nil
}

func (r *k8sProjectRepository) List(ctx context.Context) ([]models.Project, error) {
	list, err := r.platform.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: ProjectLabel,
	})
	if err != nil {
		return nil, err
	}

	projects := make([]models.Project, 0, len(list.Items))
	for i := range list.Items {
		if r.canRead(ctx, list.Items[i].Name) {
			projects = append(projects, *namespaceToProject(&list.Items[i]))
		}
	}
	return projects, nil
}

// canRead asks the API server whether the caller may get the namespace, which
// a RoleBinding in that namespace grants. Any doubt hides the project.
func (r *k8sProjectRepository) canRead(ctx context.Context, name string) bool {
	review, err := r.client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:      "get",
				Resource:  "namespaces",
				Name:      name,
				Namespace: name,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		logrus.WithError(err).WithField("project", name).Warn("Could not check access to a project, hiding it")
		return false
	}
	return review.Status.Allowed
}

// Update mutates the project metadata (currently its description) on the
// backing Namespace. Only Namespaces that are OKDP projects (carrying the
// okdp.io/project label) are updatable; anything else is reported as not found.
func (r *k8sProjectRepository) Update(ctx context.Context, project *models.Project) (*models.Project, error) {
	ns, err := r.client.CoreV1().Namespaces().Get(ctx, project.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	if ns.Labels[ProjectLabel] == "" {
		return nil, apierrors.NewNotFound(namespaceGR, project.Name)
	}

	if ns.Annotations == nil {
		ns.Annotations = map[string]string{}
	}
	ns.Annotations[ProjectDescriptionAnnot] = project.Description

	updated, err := r.client.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}

	return namespaceToProject(updated), nil
}

// Delete removes the backing Namespace. Only Namespaces that are OKDP projects
// (carrying the okdp.io/project label) are deletable. Anything else is reported
// as not found.
func (r *k8sProjectRepository) Delete(ctx context.Context, name string) error {
	ns, err := r.client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if ns.Labels[ProjectLabel] == "" {
		return apierrors.NewNotFound(namespaceGR, name)
	}

	return r.client.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
}

// Watch only passes the events of projects the caller can read. A project the
// caller loses access to is reported as deleted.
func (r *k8sProjectRepository) Watch(ctx context.Context) (watch.Interface, error) {
	w, err := r.platform.CoreV1().Namespaces().Watch(ctx, metav1.ListOptions{
		LabelSelector: ProjectLabel,
	})
	if err != nil {
		return nil, err
	}

	// watch.Filter calls this from a single goroutine.
	shown := map[string]bool{}
	return watch.Filter(w, func(event watch.Event) (watch.Event, bool) {
		ns, ok := event.Object.(*corev1.Namespace)
		if !ok {
			return event, true
		}
		if event.Type == watch.Deleted {
			wasShown := shown[ns.Name]
			delete(shown, ns.Name)
			return event, wasShown
		}

		if r.canRead(ctx, ns.Name) {
			shown[ns.Name] = true
			return event, true
		}
		if shown[ns.Name] {
			delete(shown, ns.Name)
			return watch.Event{Type: watch.Deleted, Object: ns}, true
		}
		return event, false
	}), nil
}

func namespaceToProject(ns *corev1.Namespace) *models.Project {
	return &models.Project{
		Name:        ns.Name,
		Description: ns.Annotations[ProjectDescriptionAnnot],
	}
}
