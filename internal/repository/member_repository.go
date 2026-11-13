package repository

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/okdp/okdp-control-plane-server/internal/models"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
)

const (
	// ProjectRolePrefix names the ClusterRoles of the project roles:
	// okdp-project-view, okdp-project-edit and okdp-project-admin.
	ProjectRolePrefix = "okdp-project-"
	// MemberLabel marks the RoleBindings the server creates for members.
	MemberLabel = "okdp.io/project-member"
)

var roleBindingGR = schema.GroupResource{Group: rbacv1.GroupName, Resource: "rolebindings"}

type MemberRepository interface {
	List(ctx context.Context, project string) ([]models.ProjectMember, error)
	CanManage(ctx context.Context, project string) bool
	Set(ctx context.Context, project string, member models.ProjectMember) error
	Remove(ctx context.Context, project, kind, name string) error
}

type k8sMemberRepository struct {
	client kubernetes.Interface
}

// NewMemberRepository manages project members as RoleBindings in the project
// namespace, pointing to an okdp-project-* ClusterRole. client acts as the
// caller, so the API server decides who may read or change them.
//
// Bindings created by hand count too: any RoleBinding to a project role
// makes its users and groups members.
func NewMemberRepository(client kubernetes.Interface) MemberRepository {
	return &k8sMemberRepository{client: client}
}

func (r *k8sMemberRepository) List(ctx context.Context, project string) ([]models.ProjectMember, error) {
	bindings, err := r.bindings(ctx, project)
	if err != nil {
		return nil, err
	}

	// A member bound to several roles holds the highest one.
	roles := map[models.ProjectMember]string{}
	for i := range bindings {
		role := projectRole(&bindings[i])
		for _, s := range bindings[i].Subjects {
			kind, name, ok := member(s)
			if !ok {
				continue
			}
			key := models.ProjectMember{Kind: kind, Name: name}
			if rank(role) > rank(roles[key]) {
				roles[key] = role
			}
		}
	}

	members := make([]models.ProjectMember, 0, len(roles))
	for key, role := range roles {
		key.Role = role
		members = append(members, key)
	}
	slices.SortFunc(members, func(a, b models.ProjectMember) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
	})
	return members, nil
}

// CanManage tells whether the caller may grant roles on the project. Any doubt
// answers no: the API server still has the last word on each change.
func (r *k8sMemberRepository) CanManage(ctx context.Context, project string) bool {
	review, err := r.client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:      "create",
				Group:     rbacv1.GroupName,
				Resource:  "rolebindings",
				Namespace: project,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		logrus.WithError(err).WithField("project", project).Warn("Could not check who manages a project")
		return false
	}
	return review.Status.Allowed
}

// Set gives the member this role, and only this one. The new binding is
// created before the old ones lose the member, so access never drops.
func (r *k8sMemberRepository) Set(ctx context.Context, project string, m models.ProjectMember) error {
	bindings, err := r.bindings(ctx, project)
	if err != nil {
		return err
	}
	want := subject(m.Kind, m.Name)

	granted := slices.ContainsFunc(bindings, func(rb rbacv1.RoleBinding) bool {
		return projectRole(&rb) == m.Role && hasSubject(&rb, want)
	})
	if !granted {
		_, err := r.client.RbacV1().RoleBindings(project).Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:   bindingName(m),
				Labels: map[string]string{MemberLabel: "true"},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     ProjectRolePrefix + m.Role,
			},
			Subjects: []rbacv1.Subject{want},
		}, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	}

	for i := range bindings {
		if projectRole(&bindings[i]) != m.Role && hasSubject(&bindings[i], want) {
			if err := r.removeSubject(ctx, &bindings[i], want); err != nil {
				return err
			}
		}
	}
	return nil
}

// Remove takes every project role away from the member.
func (r *k8sMemberRepository) Remove(ctx context.Context, project, kind, name string) error {
	bindings, err := r.bindings(ctx, project)
	if err != nil {
		return err
	}
	s := subject(kind, name)

	found := false
	for i := range bindings {
		if hasSubject(&bindings[i], s) {
			found = true
			if err := r.removeSubject(ctx, &bindings[i], s); err != nil {
				return err
			}
		}
	}
	if !found {
		return apierrors.NewNotFound(roleBindingGR, name)
	}
	return nil
}

// bindings returns the RoleBindings of the project that grant a project role.
func (r *k8sMemberRepository) bindings(ctx context.Context, project string) ([]rbacv1.RoleBinding, error) {
	list, err := r.client.RbacV1().RoleBindings(project).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(list.Items, func(rb rbacv1.RoleBinding) bool {
		return projectRole(&rb) == ""
	}), nil
}

// removeSubject drops the subject from the binding, and deletes the binding
// when nobody is left in it.
func (r *k8sMemberRepository) removeSubject(ctx context.Context, rb *rbacv1.RoleBinding, s rbacv1.Subject) error {
	rest := slices.DeleteFunc(slices.Clone(rb.Subjects), func(x rbacv1.Subject) bool {
		return sameSubject(x, s)
	})
	if len(rest) == 0 {
		err := r.client.RbacV1().RoleBindings(rb.Namespace).Delete(ctx, rb.Name, metav1.DeleteOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	updated := rb.DeepCopy()
	updated.Subjects = rest
	_, err := r.client.RbacV1().RoleBindings(rb.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

// projectRole returns the project role the binding grants, or "" for any
// other role.
func projectRole(rb *rbacv1.RoleBinding) string {
	if rb.RoleRef.APIGroup != rbacv1.GroupName || rb.RoleRef.Kind != "ClusterRole" {
		return ""
	}
	role, ok := strings.CutPrefix(rb.RoleRef.Name, ProjectRolePrefix)
	if !ok || !slices.Contains(models.ProjectRoles, role) {
		return ""
	}
	return role
}

// rank orders the roles: -1 for none, then view, edit, admin.
func rank(role string) int {
	return slices.Index(models.ProjectRoles, role)
}

// member returns the console user or group a subject designates. Other
// subjects, ServiceAccounts or names the server does not send, are skipped.
func member(s rbacv1.Subject) (kind, name string, ok bool) {
	name, ok = strings.CutPrefix(s.Name, CallerPrefix)
	if !ok || name == "" {
		return "", "", false
	}
	switch s.Kind {
	case rbacv1.UserKind:
		return models.MemberKindUser, name, true
	case rbacv1.GroupKind:
		return models.MemberKindGroup, name, true
	}
	return "", "", false
}

// subject is the RBAC subject of a member, named as the server impersonates it.
func subject(kind, name string) rbacv1.Subject {
	k := rbacv1.UserKind
	if kind == models.MemberKindGroup {
		k = rbacv1.GroupKind
	}
	return rbacv1.Subject{Kind: k, APIGroup: rbacv1.GroupName, Name: CallerPrefix + name}
}

func sameSubject(a, b rbacv1.Subject) bool {
	return a.Kind == b.Kind && a.Name == b.Name
}

func hasSubject(rb *rbacv1.RoleBinding, s rbacv1.Subject) bool {
	return slices.ContainsFunc(rb.Subjects, func(x rbacv1.Subject) bool { return sameSubject(x, s) })
}

// bindingName is stable for a member and a role, and valid whatever the
// characters of the name: okdp-<role>-<kind>-<hash>.
func bindingName(m models.ProjectMember) string {
	sum := sha256.Sum256([]byte(m.Kind + "/" + m.Name))
	return "okdp-" + m.Role + "-" + m.Kind + "-" + hex.EncodeToString(sum[:8])
}
