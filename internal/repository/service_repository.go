package repository

import (
	"context"
	"fmt"

	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// ServiceRepository defines Kubernetes operations for KuboCD Release resources.
// All operations take an explicit namespace so releases live in the project namespace.
type ServiceRepository interface {
	// Create returns the Release as the API server stored it.
	Create(ctx context.Context, namespace string, release *crd.Release) (*crd.Release, error)
	Get(ctx context.Context, namespace, name string) (*crd.Release, error)
	List(ctx context.Context, namespace, project string) ([]crd.Release, error)
	// Update overwrites the stored Release, whatever changed it since it was
	// read, and returns it as stored.
	Update(ctx context.Context, namespace string, release *crd.Release) (*crd.Release, error)
	// UpdateIfUnchanged writes the Release only if it is still at the
	// resourceVersion it carries, and returns it as stored. A Release changed
	// meanwhile fails with a Kubernetes Conflict error.
	UpdateIfUnchanged(ctx context.Context, namespace string, release *crd.Release) (*crd.Release, error)
	Delete(ctx context.Context, namespace, name string) error
	// DeleteIfUnchanged deletes the Release only if it is still at
	// resourceVersion. A Release changed meanwhile fails with a Kubernetes
	// Conflict error.
	DeleteIfUnchanged(ctx context.Context, namespace, name, resourceVersion string) error
	Watch(ctx context.Context, namespace, project string) (watch.Interface, error)
}

type k8sServiceRepository struct {
	client dynamic.Interface
	gvr    schema.GroupVersionResource
}

func NewServiceRepository(client dynamic.Interface) ServiceRepository {
	return &k8sServiceRepository{
		client: client,
		gvr:    crd.GetReleaseGVR(),
	}
}

func (r *k8sServiceRepository) Create(ctx context.Context, namespace string, release *crd.Release) (*crd.Release, error) {
	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(release)
	if err != nil {
		return nil, fmt.Errorf("failed to convert Release to unstructured: %w", err)
	}

	created, err := r.client.Resource(r.gvr).Namespace(namespace).Create(
		ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.CreateOptions{},
	)
	if err != nil {
		return nil, err
	}
	return fromUnstructuredRelease(created)
}

func fromUnstructuredRelease(u *unstructured.Unstructured) (*crd.Release, error) {
	var release crd.Release
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &release); err != nil {
		return nil, fmt.Errorf("failed to convert from unstructured: %w", err)
	}
	return &release, nil
}

func (r *k8sServiceRepository) Get(ctx context.Context, namespace, name string) (*crd.Release, error) {
	u, err := r.client.Resource(r.gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return fromUnstructuredRelease(u)
}

func (r *k8sServiceRepository) List(ctx context.Context, namespace, project string) ([]crd.Release, error) {
	opts := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", crd.LabelProject, project),
	}

	list, err := r.client.Resource(r.gvr).Namespace(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}

	var releases []crd.Release
	for _, item := range list.Items {
		var release crd.Release
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &release); err != nil {
			logrus.WithError(err).Warn("Failed to convert Release from unstructured")
			continue
		}
		releases = append(releases, release)
	}
	return releases, nil
}

func (r *k8sServiceRepository) Delete(ctx context.Context, namespace, name string) error {
	return r.client.Resource(r.gvr).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (r *k8sServiceRepository) DeleteIfUnchanged(ctx context.Context, namespace, name, resourceVersion string) error {
	return r.client.Resource(r.gvr).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{ResourceVersion: &resourceVersion},
	})
}

func (r *k8sServiceRepository) Update(ctx context.Context, namespace string, release *crd.Release) (*crd.Release, error) {
	existing, err := r.client.Resource(r.gvr).Namespace(namespace).Get(ctx, release.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return r.update(ctx, namespace, release, existing.GetResourceVersion())
}

func (r *k8sServiceRepository) UpdateIfUnchanged(ctx context.Context, namespace string, release *crd.Release) (*crd.Release, error) {
	return r.update(ctx, namespace, release, release.ResourceVersion)
}

func (r *k8sServiceRepository) update(ctx context.Context, namespace string, release *crd.Release, resourceVersion string) (*crd.Release, error) {
	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(release)
	if err != nil {
		return nil, fmt.Errorf("failed to convert Release to unstructured: %w", err)
	}

	obj := &unstructured.Unstructured{Object: unstructuredMap}
	obj.SetResourceVersion(resourceVersion)

	updated, err := r.client.Resource(r.gvr).Namespace(namespace).Update(ctx, obj, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	return fromUnstructuredRelease(updated)
}

func (r *k8sServiceRepository) Watch(ctx context.Context, namespace, project string) (watch.Interface, error) {
	opts := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", crd.LabelProject, project),
	}
	return r.client.Resource(r.gvr).Namespace(namespace).Watch(ctx, opts)
}
