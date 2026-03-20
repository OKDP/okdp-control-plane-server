package repository

import (
	"context"

	"github.com/okdp/okdp-server-new/internal/models"

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
	Delete(ctx context.Context, name string) error
	Watch(ctx context.Context) (watch.Interface, error)
}

type k8sProjectRepository struct {
	client kubernetes.Interface
}

// NewProjectRepository creates a project repository backed by Kubernetes Namespaces.
// A project is materialized as a Namespace carrying the label okdp.io/project=<name>
// and the annotation okdp.io/description=<description>.
func NewProjectRepository(client kubernetes.Interface) ProjectRepository {
	return &k8sProjectRepository{client: client}
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
	list, err := r.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: ProjectLabel,
	})
	if err != nil {
		return nil, err
	}

	projects := make([]models.Project, 0, len(list.Items))
	for i := range list.Items {
		projects = append(projects, *namespaceToProject(&list.Items[i]))
	}
	return projects, nil
}

func (r *k8sProjectRepository) Delete(ctx context.Context, name string) error {
	return r.client.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
}

func (r *k8sProjectRepository) Watch(ctx context.Context) (watch.Interface, error) {
	return r.client.CoreV1().Namespaces().Watch(ctx, metav1.ListOptions{
		LabelSelector: ProjectLabel,
	})
}

func namespaceToProject(ns *corev1.Namespace) *models.Project {
	return &models.Project{
		Name:        ns.Name,
		Description: ns.Annotations[ProjectDescriptionAnnot],
	}
}
