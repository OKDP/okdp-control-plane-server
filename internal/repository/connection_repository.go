package repository

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/sirupsen/logrus"
)

// ConnectionRepository accesses the KuboCD connection CRDs.
//
// Throughout, an empty namespace addresses the cluster-scoped
// ClusterConnection, mirroring the way KuboCD itself treats Connection and
// ClusterConnection as two shapes of the same thing. That keeps the
// project-scoped and the platform-wide paths on one code path.
type ConnectionRepository interface {
	// Available reports whether the connection CRDs are installed. They ship
	// with a KuboCD version the platform does not run yet, so every caller is
	// expected to check this and degrade rather than surface an error.
	Available(ctx context.Context) bool

	List(ctx context.Context, namespace string) ([]crd.Connection, error)
	Get(ctx context.Context, namespace, name string) (*crd.Connection, error)
	Create(ctx context.Context, namespace string, connection *crd.Connection) error
	Update(ctx context.Context, namespace string, connection *crd.Connection) error
	Delete(ctx context.Context, namespace, name string) error

	CreateOrUpdateSecret(ctx context.Context, namespace, name string, data map[string][]byte) error
	DeleteSecret(ctx context.Context, namespace, name string) error
	// SecretKeys returns the keys a Secret carries, so a connection pointed at
	// an existing one can be checked before it is stored. Returns false when the
	// Secret does not exist.
	SecretKeys(ctx context.Context, namespace, name string) ([]string, bool, error)
}

type k8sConnectionRepository struct {
	client        dynamic.Interface
	typedClient   kubernetes.Interface
	namespacedGVR schema.GroupVersionResource
	clusterGVR    schema.GroupVersionResource
	probe         *APIProbe
}

func NewConnectionRepository(
	client dynamic.Interface,
	typedClient kubernetes.Interface,
	discoveryClient discovery.DiscoveryInterface,
) ConnectionRepository {
	namespacedGVR := crd.GetConnectionGVR()
	return &k8sConnectionRepository{
		client:        client,
		typedClient:   typedClient,
		namespacedGVR: namespacedGVR,
		clusterGVR:    crd.GetClusterConnectionGVR(),
		probe:         NewAPIProbe(discoveryClient, namespacedGVR, "KuboCD connections"),
	}
}

// resource returns the client for the scope addressed by namespace: the
// cluster-scoped ClusterConnection when it is empty, the namespaced Connection
// otherwise.
func (r *k8sConnectionRepository) resource(namespace string) dynamic.ResourceInterface {
	if namespace == "" {
		return r.client.Resource(r.clusterGVR)
	}
	return r.client.Resource(r.namespacedGVR).Namespace(namespace)
}

func (r *k8sConnectionRepository) kind(namespace string) string {
	if namespace == "" {
		return crd.ClusterConnectionKind
	}
	return crd.ConnectionKind
}

func (r *k8sConnectionRepository) Available(ctx context.Context) bool {
	return r.probe.Available()
}

func (r *k8sConnectionRepository) List(ctx context.Context, namespace string) ([]crd.Connection, error) {
	list, err := r.resource(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	connections := make([]crd.Connection, 0, len(list.Items))
	for i := range list.Items {
		var connection crd.Connection
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &connection); err != nil {
			logrus.WithError(err).Warn("Failed to convert Connection from unstructured")
			continue
		}
		connections = append(connections, connection)
	}
	return connections, nil
}

func (r *k8sConnectionRepository) Get(ctx context.Context, namespace, name string) (*crd.Connection, error) {
	u, err := r.resource(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var connection crd.Connection
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &connection); err != nil {
		return nil, fmt.Errorf("failed to convert from unstructured: %w", err)
	}
	return &connection, nil
}

func (r *k8sConnectionRepository) Create(ctx context.Context, namespace string, connection *crd.Connection) error {
	connection.APIVersion = crd.ConnectionAPIVersion
	connection.Kind = r.kind(namespace)

	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(connection)
	if err != nil {
		return fmt.Errorf("failed to convert Connection to unstructured: %w", err)
	}

	_, err = r.resource(namespace).Create(
		ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.CreateOptions{},
	)
	return err
}

func (r *k8sConnectionRepository) Update(ctx context.Context, namespace string, connection *crd.Connection) error {
	existing, err := r.resource(namespace).Get(ctx, connection.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	connection.APIVersion = crd.ConnectionAPIVersion
	connection.Kind = r.kind(namespace)
	connection.ResourceVersion = existing.GetResourceVersion()

	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(connection)
	if err != nil {
		return fmt.Errorf("failed to convert Connection to unstructured: %w", err)
	}

	_, err = r.resource(namespace).Update(
		ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.UpdateOptions{},
	)
	return err
}

func (r *k8sConnectionRepository) Delete(ctx context.Context, namespace, name string) error {
	return r.resource(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// --- Kubernetes Secrets holding the credentials ---

func (r *k8sConnectionRepository) SecretKeys(ctx context.Context, namespace, name string) ([]string, bool, error) {
	if namespace == "" {
		return nil, false, fmt.Errorf("a namespace is required to read credentials")
	}

	secret, err := r.typedClient.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}

	// Only the key names are read, never the values: the console must be able to
	// say a Secret carries what a contract needs without ever holding it.
	keys := make([]string, 0, len(secret.Data))
	for key := range secret.Data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, true, nil
}

func (r *k8sConnectionRepository) CreateOrUpdateSecret(ctx context.Context, namespace, name string, data map[string][]byte) error {
	if namespace == "" {
		return fmt.Errorf("a namespace is required to store credentials")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{crd.LabelManagedBy: crd.ManagedByValue},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}

	existing, err := r.typedClient.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = r.typedClient.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	// Merge rather than replace. The console only resubmits the credentials the
	// user actually retyped, so a type holding several of them (a user and a
	// password) would otherwise lose the ones left untouched.
	merged := make(map[string][]byte, len(existing.Data)+len(data))
	for key, value := range existing.Data {
		merged[key] = value
	}
	for key, value := range data {
		merged[key] = value
	}
	secret.Data = merged

	secret.ResourceVersion = existing.ResourceVersion
	_, err = r.typedClient.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
	return err
}

func (r *k8sConnectionRepository) DeleteSecret(ctx context.Context, namespace, name string) error {
	err := r.typedClient.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (r *k8sConnectionRepository) ListKubeServices(ctx context.Context, namespace string) ([]corev1.Service, error) {
	list, err := r.typedClient.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}
