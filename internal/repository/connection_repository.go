package repository

import (
	"context"
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

// ConnectionSecretRepository manages the Secrets holding the credentials of
// external connections. The connections themselves are files in the
// deployments Git repository; their credentials never are: they live in a
// Secret of the project namespace, named by the connection's secretRef.
type ConnectionSecretRepository interface {
	CreateOrUpdateSecret(ctx context.Context, namespace, name string, data map[string][]byte) error
	DeleteSecret(ctx context.Context, namespace, name string) error
	// InspectSecret returns what an existing Secret exposes to the checks a
	// connection runs before it is stored. Returns false when the Secret does
	// not exist.
	InspectSecret(ctx context.Context, namespace, name string) (SecretContent, bool, error)
}

// SecretContent is what a Secret exposes to those checks: the keys it carries,
// and whether the control plane owns it.
type SecretContent struct {
	Keys    []string
	Managed bool
}

type k8sConnectionSecretRepository struct {
	typedClient kubernetes.Interface
}

func NewConnectionSecretRepository(typedClient kubernetes.Interface) ConnectionSecretRepository {
	return &k8sConnectionSecretRepository{typedClient: typedClient}
}

// --- Kubernetes Secrets holding the credentials ---

func (r *k8sConnectionSecretRepository) InspectSecret(ctx context.Context, namespace, name string) (SecretContent, bool, error) {
	if namespace == "" {
		return SecretContent{}, false, fmt.Errorf("a namespace is required to read credentials")
	}

	secret, err := r.typedClient.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return SecretContent{}, false, nil
		}
		return SecretContent{}, false, err
	}

	// Only the key names are read, never the values: the console must be able to
	// say a Secret carries what a contract needs without ever holding it.
	keys := make([]string, 0, len(secret.Data))
	for key := range secret.Data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return SecretContent{
		Keys:    keys,
		Managed: secret.Labels[crd.LabelManagedBy] == crd.ManagedByValue,
	}, true, nil
}

// ErrForeignSecret is returned when the credentials name is already taken by a
// Secret this server does not own. Adopting it would put someone else's data
// under our lifecycle.
var ErrForeignSecret = errors.New("the credentials secret already exists and is not managed by the control plane")

func (r *k8sConnectionSecretRepository) CreateOrUpdateSecret(ctx context.Context, namespace, name string, data map[string][]byte) error {
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

	// The credentials name is derived from the connection name, so a Secret
	// someone else put there is reachable by picking that name. Writing into it
	// would also make us its owner, and the delete path would take it away with
	// the connection.
	if existing.Labels[crd.LabelManagedBy] != crd.ManagedByValue {
		return fmt.Errorf("%w: secret %q in namespace %q", ErrForeignSecret, name, namespace)
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

func (r *k8sConnectionSecretRepository) DeleteSecret(ctx context.Context, namespace, name string) error {
	err := r.typedClient.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
