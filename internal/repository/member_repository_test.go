package repository

import (
	"context"
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

func roleBinding(name, clusterRole string, subjects ...rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole},
		Subjects:   subjects,
	}
}

func group(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: name}
}

func user(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}
}

func bindingsIn(t *testing.T, client *k8sfake.Clientset) map[string]*rbacv1.RoleBinding {
	t.Helper()
	list, err := client.RbacV1().RoleBindings("demo").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list role bindings: %v", err)
	}
	byName := map[string]*rbacv1.RoleBinding{}
	for i := range list.Items {
		byName[list.Items[i].Name] = &list.Items[i]
	}
	return byName
}

func TestListMembersReadsTheBindingsToProjectRoles(t *testing.T) {
	client := k8sfake.NewSimpleClientset(
		roleBinding("by-hand", "okdp-project-edit", group("oidc:data_scientist"), user("oidc:bob"),
			rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: "demo"}),
		roleBinding("bob-admin", "okdp-project-admin", user("oidc:bob")),
		roleBinding("other-role", "view", user("oidc:nina")),
		roleBinding("no-prefix", "okdp-project-view", user("mark")),
	)

	members, err := NewMemberRepository(client).List(context.Background(), "demo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []models.ProjectMember{
		{Kind: "group", Name: "data_scientist", Role: "edit"},
		{Kind: "user", Name: "bob", Role: "admin"},
	}
	if !reflect.DeepEqual(members, want) {
		t.Fatalf("members = %v, want %v", members, want)
	}
}

func TestSetMemberCreatesABinding(t *testing.T) {
	client := k8sfake.NewSimpleClientset()

	err := NewMemberRepository(client).Set(context.Background(), "demo", models.ProjectMember{Kind: "group", Name: "data_engineer", Role: "edit"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bindings := bindingsIn(t, client)
	if len(bindings) != 1 {
		t.Fatalf("bindings = %v, want one", bindings)
	}
	for _, rb := range bindings {
		if rb.RoleRef.Name != "okdp-project-edit" || rb.RoleRef.Kind != "ClusterRole" {
			t.Errorf("roleRef = %+v, want ClusterRole okdp-project-edit", rb.RoleRef)
		}
		if !reflect.DeepEqual(rb.Subjects, []rbacv1.Subject{group("oidc:data_engineer")}) {
			t.Errorf("subjects = %+v, want the prefixed group", rb.Subjects)
		}
		if rb.Labels[MemberLabel] != "true" {
			t.Errorf("labels = %v, want %s", rb.Labels, MemberLabel)
		}
	}
}

func TestSetMemberTwiceKeepsOneBinding(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	repo := NewMemberRepository(client)
	member := models.ProjectMember{Kind: "user", Name: "mark@example.org", Role: "view"}

	for range 2 {
		if err := repo.Set(context.Background(), "demo", member); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if bindings := bindingsIn(t, client); len(bindings) != 1 {
		t.Fatalf("bindings = %v, want one", bindings)
	}
}

func TestSetMemberReplacesTheFormerRole(t *testing.T) {
	client := k8sfake.NewSimpleClientset(
		roleBinding("mark-view", "okdp-project-view", user("oidc:mark")),
		roleBinding("team", "okdp-project-edit", user("oidc:mark"), user("oidc:bob")),
	)

	err := NewMemberRepository(client).Set(context.Background(), "demo", models.ProjectMember{Kind: "user", Name: "mark", Role: "admin"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bindings := bindingsIn(t, client)
	if _, ok := bindings["mark-view"]; ok {
		t.Errorf("mark-view should be deleted once empty")
	}
	if team := bindings["team"]; team == nil || !reflect.DeepEqual(team.Subjects, []rbacv1.Subject{user("oidc:bob")}) {
		t.Errorf("team = %+v, want bob only", team)
	}
	members, _ := NewMemberRepository(client).List(context.Background(), "demo")
	want := []models.ProjectMember{{Kind: "user", Name: "bob", Role: "edit"}, {Kind: "user", Name: "mark", Role: "admin"}}
	if !reflect.DeepEqual(members, want) {
		t.Fatalf("members = %v, want %v", members, want)
	}
}

func TestRemoveMemberTakesEveryRoleAway(t *testing.T) {
	client := k8sfake.NewSimpleClientset(
		roleBinding("mark-view", "okdp-project-view", user("oidc:mark")),
		roleBinding("team", "okdp-project-edit", user("oidc:mark"), group("oidc:mark")),
		roleBinding("other-role", "view", user("oidc:mark")),
	)

	if err := NewMemberRepository(client).Remove(context.Background(), "demo", "user", "mark"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bindings := bindingsIn(t, client)
	if _, ok := bindings["mark-view"]; ok {
		t.Errorf("mark-view should be deleted")
	}
	if team := bindings["team"]; team == nil || !reflect.DeepEqual(team.Subjects, []rbacv1.Subject{group("oidc:mark")}) {
		t.Errorf("team = %+v, want the group left alone", team)
	}
	if _, ok := bindings["other-role"]; !ok {
		t.Errorf("a binding to another role is not a membership and must stay")
	}
}

func TestRemoveUnknownMemberIsNotFound(t *testing.T) {
	client := k8sfake.NewSimpleClientset()

	err := NewMemberRepository(client).Remove(context.Background(), "demo", "group", "nobody")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}
