package repository

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

func secretRepo(secrets ...*corev1.Secret) *k8sConnectionRepository {
	objects := make([]runtime.Object, 0, len(secrets))
	for _, secret := range secrets {
		objects = append(objects, secret)
	}
	return &k8sConnectionRepository{typedClient: k8sfake.NewSimpleClientset(objects...)}
}

// The credentials name is derived from the connection name, so anyone can park
// a Secret there. Writing into it would also hand it our lifecycle, and the
// delete path would take it away with the connection.
func TestCreateOrUpdateSecretRefusesAForeignSecret(t *testing.T) {
	repo := secretRepo(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "warehouse-credentials", Namespace: "demo"},
		Data:       map[string][]byte{"password": []byte("someone-elses")},
	})

	err := repo.CreateOrUpdateSecret(context.Background(), "demo", "warehouse-credentials", map[string][]byte{"password": []byte("ours")})

	if !errors.Is(err, ErrForeignSecret) {
		t.Fatalf("expected the adoption to be refused, got %v", err)
	}

	kept, getErr := repo.typedClient.CoreV1().Secrets("demo").Get(context.Background(), "warehouse-credentials", metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("the foreign secret should still be there: %v", getErr)
	}
	if string(kept.Data["password"]) != "someone-elses" {
		t.Fatalf("the foreign secret was written into: %q", kept.Data["password"])
	}
}

// One we own is merged, so an edit that resubmits only the password keeps the
// username stored next to it.
func TestCreateOrUpdateSecretMergesOneWeOwn(t *testing.T) {
	repo := secretRepo(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warehouse-credentials",
			Namespace: "demo",
			Labels:    map[string]string{crd.LabelManagedBy: crd.ManagedByValue},
		},
		Data: map[string][]byte{"username": []byte("admin"), "password": []byte("old")},
	})

	if err := repo.CreateOrUpdateSecret(context.Background(), "demo", "warehouse-credentials", map[string][]byte{"password": []byte("new")}); err != nil {
		t.Fatalf("expected the update to go through, got %v", err)
	}

	stored, err := repo.typedClient.CoreV1().Secrets("demo").Get(context.Background(), "warehouse-credentials", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Data["password"]) != "new" || string(stored.Data["username"]) != "admin" {
		t.Fatalf("expected the password updated and the username kept, got %v", stored.Data)
	}
}
