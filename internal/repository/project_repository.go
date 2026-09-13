package repository

import (
	"context"

	"github.com/okdp/okdp-control-plane-server/internal/models"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ProjectLabel marks a Namespace the console created for a project. The
	// projects themselves are the project.yaml files of the deployments
	// repository; the label only says the namespace is the console's to delete.
	ProjectLabel            = "okdp.io/project"
	ProjectDescriptionAnnot = "okdp.io/description"
)

// ProjectNamespaceRepository manages the Namespace a project deploys into.
// The project itself is declared in Git (projects/<p>/project.yaml); its
// Namespace is runtime state: the engines create it on the first
// deployment, and the console creates it up front so a new project can hold
// Secrets before any service is deployed.
type ProjectNamespaceRepository interface {
	// CreateNamespace creates the Namespace of a new project, labelled
	// okdp.io/project. created is false when a Namespace labelled for this
	// project already exists. A Namespace that exists without the label is not
	// a project's (kube-system, a platform namespace): AlreadyExists.
	CreateNamespace(ctx context.Context, project *models.Project) (created bool, err error)
	// DeleteNamespace deletes the Namespace of a project when the console
	// created it (it carries the okdp.io/project label). A missing Namespace,
	// or one without the label, is left alone: deleted is false.
	DeleteNamespace(ctx context.Context, name string) (deleted bool, err error)
}

type k8sProjectNamespaceRepository struct {
	client kubernetes.Interface
}

// NewProjectNamespaceRepository manages project Namespaces through the Kubernetes API.
func NewProjectNamespaceRepository(client kubernetes.Interface) ProjectNamespaceRepository {
	return &k8sProjectNamespaceRepository{client: client}
}

func (r *k8sProjectNamespaceRepository) CreateNamespace(ctx context.Context, project *models.Project) (bool, error) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        project.Name,
			Labels:      map[string]string{ProjectLabel: project.Name},
			Annotations: map[string]string{ProjectDescriptionAnnot: project.Description},
		},
	}
	_, err := r.client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err == nil {
		return true, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return false, err
	}
	existing, getErr := r.client.CoreV1().Namespaces().Get(ctx, project.Name, metav1.GetOptions{})
	if getErr != nil {
		return false, getErr
	}
	if existing.Labels[ProjectLabel] == "" {
		return false, err
	}
	return false, nil
}

func (r *k8sProjectNamespaceRepository) DeleteNamespace(ctx context.Context, name string) (bool, error) {
	ns, err := r.client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ns.Labels[ProjectLabel] == "" {
		return false, nil
	}
	err = r.client.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}
