package repository

import (
	"context"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func TestCreateNamespaceLabelsANewProjectNamespace(t *testing.T) {
	client := k8sfake.NewSimpleClientset(namespace("old", map[string]string{ProjectLabel: "old"}))
	repo := NewProjectNamespaceRepository(client)
	ctx := context.Background()

	created, err := repo.CreateNamespace(ctx, &models.Project{Name: "demo", Description: "Demo"})
	if err != nil || !created {
		t.Fatalf("create demo: created=%v err=%v", created, err)
	}
	ns, err := client.CoreV1().Namespaces().Get(ctx, "demo", metav1.GetOptions{})
	if err != nil || ns.Labels[ProjectLabel] != "demo" || ns.Annotations[ProjectDescriptionAnnot] != "Demo" {
		t.Fatalf("namespace demo = %+v, %v", ns, err)
	}

	created, err = repo.CreateNamespace(ctx, &models.Project{Name: "old"})
	if err != nil || created {
		t.Fatalf("a project namespace left behind is reused: created=%v err=%v", created, err)
	}
}

func TestCreateNamespaceRefusesANamespaceThatIsNotAProject(t *testing.T) {
	client := k8sfake.NewSimpleClientset(namespace("kube-system", nil))
	repo := NewProjectNamespaceRepository(client)

	created, err := repo.CreateNamespace(context.Background(), &models.Project{Name: "kube-system"})
	if !apierrors.IsAlreadyExists(err) || created {
		t.Fatalf("expected already-exists, got created=%v err=%v", created, err)
	}
}

func TestDeleteNamespaceRemovesAProjectNamespace(t *testing.T) {
	client := k8sfake.NewSimpleClientset(namespace("demo", map[string]string{ProjectLabel: "demo"}))
	repo := NewProjectNamespaceRepository(client)

	deleted, err := repo.DeleteNamespace(context.Background(), "demo")
	if err != nil || !deleted {
		t.Fatalf("expected the namespace to be deleted, got deleted=%v err=%v", deleted, err)
	}
	if _, err := client.CoreV1().Namespaces().Get(context.Background(), "demo", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected the namespace to be gone, got %v", err)
	}
}

func TestDeleteNamespaceLeavesOtherNamespacesAlone(t *testing.T) {
	// kube-system is not a project's; demo was created by a GitOps engine for
	// a project written in Git, without the label.
	client := k8sfake.NewSimpleClientset(namespace("kube-system", nil), namespace("demo", nil))
	repo := NewProjectNamespaceRepository(client)

	for _, name := range []string{"kube-system", "demo", "missing"} {
		deleted, err := repo.DeleteNamespace(context.Background(), name)
		if err != nil || deleted {
			t.Fatalf("%s: deleted=%v err=%v", name, deleted, err)
		}
	}
	for _, name := range []string{"kube-system", "demo"} {
		if _, err := client.CoreV1().Namespaces().Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			t.Fatalf("expected %s to survive, got %v", name, err)
		}
	}
}
