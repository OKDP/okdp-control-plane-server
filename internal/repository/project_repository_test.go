package repository

import (
	"context"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func TestDeleteRemovesAProject(t *testing.T) {
	client := k8sfake.NewSimpleClientset(namespace("demo", map[string]string{ProjectLabel: "true"}))
	repo := NewProjectRepository(client, client)

	if err := repo.Delete(context.Background(), "demo"); err != nil {
		t.Fatalf("expected the project to be deleted, got %v", err)
	}

	if _, err := client.CoreV1().Namespaces().Get(context.Background(), "demo", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected the namespace to be gone, got %v", err)
	}
}

func TestDeleteRefusesANamespaceThatIsNotAProject(t *testing.T) {
	client := k8sfake.NewSimpleClientset(namespace("kube-system", nil))
	repo := NewProjectRepository(client, client)

	err := repo.Delete(context.Background(), "kube-system")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected a not-found error, got %v", err)
	}

	if _, err := client.CoreV1().Namespaces().Get(context.Background(), "kube-system", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected the namespace to survive, got %v", err)
	}
}

// callerAllowedOn answers SelfSubjectAccessReviews as RBAC would for a caller
// bound in the given namespaces only.
func callerAllowedOn(namespaces ...string) *k8sfake.Clientset {
	allowed := map[string]bool{}
	for _, ns := range namespaces {
		allowed[ns] = true
	}
	client := k8sfake.NewSimpleClientset()
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		review.Status.Allowed = allowed[review.Spec.ResourceAttributes.Namespace]
		return true, review, nil
	})
	return client
}

func TestListShowsOnlyTheProjectsTheCallerCanRead(t *testing.T) {
	platform := k8sfake.NewSimpleClientset(
		namespace("france-data", map[string]string{ProjectLabel: "france-data"}),
		namespace("finance", map[string]string{ProjectLabel: "finance"}),
	)
	repo := NewProjectRepository(callerAllowedOn("france-data"), platform)

	projects, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projects) != 1 || projects[0].Name != "france-data" {
		t.Fatalf("projects = %v, want only france-data", projects)
	}
}

func TestWatchHidesTheProjectsTheCallerCannotRead(t *testing.T) {
	platform := k8sfake.NewSimpleClientset()
	repo := NewProjectRepository(callerAllowedOn("france-data"), platform)

	w, err := repo.Watch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer w.Stop()

	for _, name := range []string{"finance", "france-data"} {
		if _, err := platform.CoreV1().Namespaces().Create(context.Background(), namespace(name, map[string]string{ProjectLabel: name}), metav1.CreateOptions{}); err != nil {
			t.Fatalf("could not create %s: %v", name, err)
		}
	}

	event := nextEvent(t, w)
	if ns := event.Object.(*corev1.Namespace); event.Type != watch.Added || ns.Name != "france-data" {
		t.Fatalf("event = %s %s, want ADDED france-data", event.Type, ns.Name)
	}
}

func nextEvent(t *testing.T, w watch.Interface) watch.Event {
	t.Helper()
	select {
	case event := <-w.ResultChan():
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("no event received")
		return watch.Event{}
	}
}
