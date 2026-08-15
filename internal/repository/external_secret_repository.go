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
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

// ExternalSecretRepository defines Kubernetes operations for ESO ExternalSecret resources.
// Namespace is passed per call (project = namespace).
type ExternalSecretRepository interface {
	// Available reports whether the external-secrets CRDs are installed.
	Available(ctx context.Context) bool

	Create(ctx context.Context, namespace string, es *crd.ESOExternalSecret) error
	Get(ctx context.Context, namespace, name string) (*crd.ESOExternalSecret, error)
	List(ctx context.Context, namespace string) ([]crd.ESOExternalSecret, error)
	Update(ctx context.Context, namespace string, es *crd.ESOExternalSecret) error
	Delete(ctx context.Context, namespace, name string) error
}

type k8sExternalSecretRepository struct {
	client dynamic.Interface
	gvr    schema.GroupVersionResource
	probe  *APIProbe
}

func NewExternalSecretRepository(client dynamic.Interface, discoveryClient discovery.DiscoveryInterface) ExternalSecretRepository {
	gvr := crd.GetExternalSecretGVR()
	return &k8sExternalSecretRepository{
		client: client,
		gvr:    gvr,
		probe:  NewAPIProbe(discoveryClient, gvr, "external-secrets"),
	}
}

func (r *k8sExternalSecretRepository) Available(ctx context.Context) bool {
	return r.probe.Available()
}

func (r *k8sExternalSecretRepository) Create(ctx context.Context, namespace string, es *crd.ESOExternalSecret) error {
	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(es)
	if err != nil {
		return fmt.Errorf("failed to convert ExternalSecret to unstructured: %w", err)
	}

	_, err = r.client.Resource(r.gvr).Namespace(namespace).Create(
		ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.CreateOptions{},
	)
	return err
}

func (r *k8sExternalSecretRepository) Get(ctx context.Context, namespace, name string) (*crd.ESOExternalSecret, error) {
	u, err := r.client.Resource(r.gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var es crd.ESOExternalSecret
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &es); err != nil {
		return nil, fmt.Errorf("failed to convert from unstructured: %w", err)
	}
	return &es, nil
}

func (r *k8sExternalSecretRepository) List(ctx context.Context, namespace string) ([]crd.ESOExternalSecret, error) {
	list, err := r.client.Resource(r.gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var items []crd.ESOExternalSecret
	for _, item := range list.Items {
		var es crd.ESOExternalSecret
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &es); err != nil {
			logrus.WithError(err).Warn("Failed to convert ExternalSecret from unstructured")
			continue
		}
		items = append(items, es)
	}
	return items, nil
}

func (r *k8sExternalSecretRepository) Update(ctx context.Context, namespace string, es *crd.ESOExternalSecret) error {
	existing, err := r.client.Resource(r.gvr).Namespace(namespace).Get(ctx, es.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	es.ResourceVersion = existing.GetResourceVersion()

	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(es)
	if err != nil {
		return fmt.Errorf("failed to convert ExternalSecret to unstructured: %w", err)
	}

	_, err = r.client.Resource(r.gvr).Namespace(namespace).Update(
		ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.UpdateOptions{},
	)
	return err
}

func (r *k8sExternalSecretRepository) Delete(ctx context.Context, namespace, name string) error {
	return r.client.Resource(r.gvr).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}
