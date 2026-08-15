package repository

import (
	"context"
	"fmt"

	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

// SecretStoreRepository defines Kubernetes operations for ESO SecretStore resources.
// Unlike other repositories, namespace is passed per call (project = namespace).
type SecretStoreRepository interface {
	// Available reports whether the external-secrets CRDs are installed. A
	// cluster without them carries no vault integration at all, which is a
	// deployment choice rather than a failure.
	Available(ctx context.Context) bool

	Create(ctx context.Context, namespace string, store *crd.ESOSecretStore) error
	Get(ctx context.Context, namespace, name string) (*crd.ESOSecretStore, error)
	List(ctx context.Context, namespace string) ([]crd.ESOSecretStore, error)
	Update(ctx context.Context, namespace string, store *crd.ESOSecretStore) error
	Delete(ctx context.Context, namespace, name string) error

	CreateOrUpdateSecret(ctx context.Context, namespace, name string, data map[string][]byte) error
	GetSecretData(ctx context.Context, namespace, name string) (map[string][]byte, error)
	DeleteSecret(ctx context.Context, namespace, name string) error

	// RemoveDefaultLabel removes the default-store label from all SecretStores in the namespace
	RemoveDefaultLabel(ctx context.Context, namespace string) error
}

type k8sSecretStoreRepository struct {
	client dynamic.Interface
	gvr    schema.GroupVersionResource
	probe  *APIProbe
}

func NewSecretStoreRepository(client dynamic.Interface, discoveryClient discovery.DiscoveryInterface) SecretStoreRepository {
	gvr := crd.GetSecretStoreGVR()
	return &k8sSecretStoreRepository{
		client: client,
		gvr:    gvr,
		probe:  NewAPIProbe(discoveryClient, gvr, "external-secrets"),
	}
}

func (r *k8sSecretStoreRepository) Available(ctx context.Context) bool {
	return r.probe.Available()
}

func (r *k8sSecretStoreRepository) Create(ctx context.Context, namespace string, store *crd.ESOSecretStore) error {
	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(store)
	if err != nil {
		return fmt.Errorf("failed to convert SecretStore to unstructured: %w", err)
	}

	_, err = r.client.Resource(r.gvr).Namespace(namespace).Create(
		ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.CreateOptions{},
	)
	return err
}

func (r *k8sSecretStoreRepository) Get(ctx context.Context, namespace, name string) (*crd.ESOSecretStore, error) {
	u, err := r.client.Resource(r.gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var store crd.ESOSecretStore
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &store); err != nil {
		return nil, fmt.Errorf("failed to convert from unstructured: %w", err)
	}
	return &store, nil
}

func (r *k8sSecretStoreRepository) List(ctx context.Context, namespace string) ([]crd.ESOSecretStore, error) {
	list, err := r.client.Resource(r.gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var stores []crd.ESOSecretStore
	for _, item := range list.Items {
		var store crd.ESOSecretStore
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &store); err != nil {
			logrus.WithError(err).Warn("Failed to convert SecretStore from unstructured")
			continue
		}
		stores = append(stores, store)
	}
	return stores, nil
}

func (r *k8sSecretStoreRepository) Update(ctx context.Context, namespace string, store *crd.ESOSecretStore) error {
	existing, err := r.client.Resource(r.gvr).Namespace(namespace).Get(ctx, store.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	store.ResourceVersion = existing.GetResourceVersion()

	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(store)
	if err != nil {
		return fmt.Errorf("failed to convert SecretStore to unstructured: %w", err)
	}

	_, err = r.client.Resource(r.gvr).Namespace(namespace).Update(
		ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.UpdateOptions{},
	)
	return err
}

func (r *k8sSecretStoreRepository) Delete(ctx context.Context, namespace, name string) error {
	return r.client.Resource(r.gvr).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// --- Kubernetes Secrets for credentials ---

var secretGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

func (r *k8sSecretStoreRepository) CreateOrUpdateSecret(ctx context.Context, namespace, name string, data map[string][]byte) error {
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}

	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(secret)
	if err != nil {
		return fmt.Errorf("failed to convert secret to unstructured: %w", err)
	}

	u := &unstructured.Unstructured{Object: unstructuredMap}

	_, getErr := r.client.Resource(secretGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(getErr) {
		_, err = r.client.Resource(secretGVR).Namespace(namespace).Create(ctx, u, metav1.CreateOptions{})
	} else if getErr != nil {
		return getErr
	} else {
		_, err = r.client.Resource(secretGVR).Namespace(namespace).Update(ctx, u, metav1.UpdateOptions{})
	}
	return err
}

func (r *k8sSecretStoreRepository) GetSecretData(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	u, err := r.client.Resource(secretGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var secret corev1.Secret
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &secret); err != nil {
		return nil, fmt.Errorf("failed to convert from unstructured: %w", err)
	}
	return secret.Data, nil
}

func (r *k8sSecretStoreRepository) DeleteSecret(ctx context.Context, namespace, name string) error {
	return r.client.Resource(secretGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// RemoveDefaultLabel lists all SecretStores with the default label and removes it.
func (r *k8sSecretStoreRepository) RemoveDefaultLabel(ctx context.Context, namespace string) error {
	list, err := r.client.Resource(r.gvr).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: crd.LabelDefaultStore + "=true",
	})
	if err != nil {
		return err
	}

	for _, item := range list.Items {
		labels := item.GetLabels()
		delete(labels, crd.LabelDefaultStore)
		item.SetLabels(labels)

		_, err := r.client.Resource(r.gvr).Namespace(namespace).Update(ctx, &item, metav1.UpdateOptions{})
		if err != nil {
			logrus.WithError(err).Warnf("Failed to remove default label from SecretStore %s", item.GetName())
		}
	}
	return nil
}
