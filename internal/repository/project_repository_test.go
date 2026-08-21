package repository

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func TestDeleteRemovesAProject(t *testing.T) {
	client := k8sfake.NewSimpleClientset(namespace("demo", map[string]string{ProjectLabel: "true"}))
	repo := NewProjectRepository(client)

	if err := repo.Delete(context.Background(), "demo"); err != nil {
		t.Fatalf("expected the project to be deleted, got %v", err)
	}

	if _, err := client.CoreV1().Namespaces().Get(context.Background(), "demo", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected the namespace to be gone, got %v", err)
	}
}

func TestDeleteRefusesANamespaceThatIsNotAProject(t *testing.T) {
	client := k8sfake.NewSimpleClientset(namespace("kube-system", nil))
	repo := NewProjectRepository(client)

	err := repo.Delete(context.Background(), "kube-system")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected a not-found error, got %v", err)
	}

	if _, err := client.CoreV1().Namespaces().Get(context.Background(), "kube-system", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected the namespace to survive, got %v", err)
	}
}
