package repository

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

func storeSecretRepo(secrets ...*corev1.Secret) *k8sSecretStoreRepository {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	objects := make([]runtime.Object, 0, len(secrets))
	for _, secret := range secrets {
		objects = append(objects, secret)
	}
	return &k8sSecretStoreRepository{client: dynamicfake.NewSimpleDynamicClient(scheme, objects...)}
}

func storeSecret(t *testing.T, repo *k8sSecretStoreRepository, name string) *corev1.Secret {
	t.Helper()
	u, err := repo.client.Resource(secretGVR).Namespace("demo").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	var secret corev1.Secret
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &secret); err != nil {
		t.Fatal(err)
	}
	return &secret
}

// A store named p-hive would write <store>-credentials = p-hive-credentials,
// the contract Secret of the hive instance p-hive.
func TestStoreCredentialsNeverOverwriteOrDeleteAForeignSecret(t *testing.T) {
	repo := storeSecretRepo(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "p-hive-credentials", Namespace: "demo"},
		Data:       map[string][]byte{"password": []byte("hive")},
	})
	ctx := context.Background()

	err := repo.CreateOrUpdateSecret(ctx, "demo", "p-hive-credentials", map[string][]byte{"token": []byte("vault")})
	if !errors.Is(err, ErrForeignSecret) {
		t.Fatalf("write: %v, want ErrForeignSecret", err)
	}
	if err := repo.DeleteSecret(ctx, "demo", "p-hive-credentials"); !errors.Is(err, ErrForeignSecret) {
		t.Fatalf("delete: %v, want ErrForeignSecret", err)
	}
	if kept := storeSecret(t, repo, "p-hive-credentials"); string(kept.Data["password"]) != "hive" || kept.Data["token"] != nil {
		t.Fatalf("the foreign secret was changed: %v", kept.Data)
	}
}

func TestStoreCredentialsAreLabelledAndManagedAsOurs(t *testing.T) {
	repo := storeSecretRepo()
	ctx := context.Background()

	if err := repo.CreateOrUpdateSecret(ctx, "demo", "vault-credentials", map[string][]byte{"token": []byte("one")}); err != nil {
		t.Fatal(err)
	}
	if got := storeSecret(t, repo, "vault-credentials").Labels[crd.LabelManagedBy]; got != crd.ManagedByValue {
		t.Fatalf("managed-by label = %q", got)
	}
	if err := repo.CreateOrUpdateSecret(ctx, "demo", "vault-credentials", map[string][]byte{"token": []byte("two")}); err != nil {
		t.Fatalf("rotating our own token: %v", err)
	}
	if got := string(storeSecret(t, repo, "vault-credentials").Data["token"]); got != "two" {
		t.Fatalf("token = %q", got)
	}
	if err := repo.DeleteSecret(ctx, "demo", "vault-credentials"); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteSecret(ctx, "demo", "vault-credentials"); !apierrors.IsNotFound(err) {
		t.Fatalf("second delete: %v, want NotFound", err)
	}
}
