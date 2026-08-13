package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

type IdentityRepository interface {
	// Available reports whether the kubauth CRDs are installed on this cluster.
	Available(ctx context.Context) bool

	// Users
	ListUsers(ctx context.Context) ([]models.User, error)
	GetUser(ctx context.Context, name string) (*models.User, error)
	CreateUser(ctx context.Context, user *crd.User) error
	UpdateUser(ctx context.Context, user *crd.User) error
	DeleteUser(ctx context.Context, name string) error

	// Groups
	ListGroups(ctx context.Context) ([]models.Group, error)
	GetGroup(ctx context.Context, name string) (*models.Group, error)
	CreateGroup(ctx context.Context, group *crd.Group) error
	UpdateGroup(ctx context.Context, group *crd.Group) error
	DeleteGroup(ctx context.Context, name string) error

	// GroupBindings
	ListGroupBindings(ctx context.Context, userFilter string) ([]models.GroupBinding, error)
	CreateGroupBinding(ctx context.Context, binding *crd.GroupBinding) error
	DeleteGroupBinding(ctx context.Context, name string) error
	DeleteGroupBindingByRef(ctx context.Context, user, group string) error
}

type k8sIdentityRepository struct {
	client    dynamic.Interface
	namespace func(ctx context.Context) string
	userGVR   schema.GroupVersionResource
	groupGVR  schema.GroupVersionResource
	gbGVR     schema.GroupVersionResource
	probe     *APIProbe
}

func NewIdentityRepository(client dynamic.Interface, discoveryClient discovery.DiscoveryInterface, namespace func(ctx context.Context) string) IdentityRepository {
	groupName := "kubauth.kubotal.io"
	version := "v1alpha1"

	userGVR := schema.GroupVersionResource{
		Group:    groupName,
		Version:  version,
		Resource: "users",
	}

	return &k8sIdentityRepository{
		client:    client,
		namespace: namespace,
		userGVR:   userGVR,
		groupGVR: schema.GroupVersionResource{
			Group:    groupName,
			Version:  version,
			Resource: "groups",
		},
		gbGVR: schema.GroupVersionResource{
			Group:    groupName,
			Version:  version,
			Resource: "groupbindings",
		},
		probe: NewAPIProbe(discoveryClient, userGVR, "kubauth identity"),
	}
}

// Available reports whether the kubauth CRDs are installed. The identity routes
// rest on it, so an installation without kubauth answers 501 rather than failing
// deeper.
func (r *k8sIdentityRepository) Available(ctx context.Context) bool {
	return r.probe.Available()
}

// --- Users ---

func (r *k8sIdentityRepository) ListUsers(ctx context.Context) ([]models.User, error) {
	list, err := r.client.Resource(r.userGVR).Namespace(r.namespace(ctx)).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var users []models.User
	for _, item := range list.Items {
		var crdObj crd.User
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &crdObj); err != nil {
			logrus.WithError(err).Warn("Failed to convert user from unstructured")
			continue
		}

		uid := 0
		if crdObj.Spec.Uid != nil {
			uid = *crdObj.Spec.Uid
		}

		disabled := false
		if crdObj.Spec.Disabled != nil {
			disabled = *crdObj.Spec.Disabled
		}

		users = append(users, models.User{
			Username: item.GetName(),
			Name:     crdObj.Spec.Name,
			Email:    crdObj.Spec.Emails,
			Comment:  crdObj.Spec.Comment,
			UID:      uid,
			Disabled: disabled,
		})
	}
	return users, nil
}

func (r *k8sIdentityRepository) GetUser(ctx context.Context, name string) (*models.User, error) {
	u, err := r.client.Resource(r.userGVR).Namespace(r.namespace(ctx)).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var crdObj crd.User
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &crdObj); err != nil {
		return nil, fmt.Errorf("failed to convert from unstructured: %w", err)
	}

	uid := 0
	if crdObj.Spec.Uid != nil {
		uid = *crdObj.Spec.Uid
	}

	disabled := false
	if crdObj.Spec.Disabled != nil {
		disabled = *crdObj.Spec.Disabled
	}

	return &models.User{
		Username:     u.GetName(),
		Name:         crdObj.Spec.Name,
		Email:        crdObj.Spec.Emails,
		Comment:      crdObj.Spec.Comment,
		UID:          uid,
		Disabled:     disabled,
		PasswordHash: crdObj.Spec.PasswordHash,
	}, nil
}

func (r *k8sIdentityRepository) CreateUser(ctx context.Context, user *crd.User) error {
	user.TypeMeta = metav1.TypeMeta{
		APIVersion: "kubauth.kubotal.io/v1alpha1",
		Kind:       "User",
	}
	// Name in metadata must match spec.name usually for Kubauth or at least be the resource name
	if user.ObjectMeta.Name == "" {
		user.ObjectMeta.Name = user.Spec.Name
	}

	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(user)
	if err != nil {
		return fmt.Errorf("failed to convert to unstructured: %w", err)
	}

	_, err = r.client.Resource(r.userGVR).Namespace(r.namespace(ctx)).Create(ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.CreateOptions{})
	return err
}

func (r *k8sIdentityRepository) UpdateUser(ctx context.Context, user *crd.User) error {
	// We need to fetch the existing resource first to get the resourceVersion
	// Or we can rely on the caller to provide a full object with resourceVersion if they did a GET before.
	// For simplicity, let's assume we might need to handle resourceVersion collision or just overwrite if provided.
	// But usually Update requires ResourceVersion.
	// If the passed user object doesn't have ResourceVersion, we should probably GET it first.

	current, err := r.client.Resource(r.userGVR).Namespace(r.namespace(ctx)).Get(ctx, user.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	user.SetResourceVersion(current.GetResourceVersion())
	user.TypeMeta = metav1.TypeMeta{
		APIVersion: "kubauth.kubotal.io/v1alpha1",
		Kind:       "User",
	}

	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(user)
	if err != nil {
		return fmt.Errorf("failed to convert to unstructured: %w", err)
	}

	_, err = r.client.Resource(r.userGVR).Namespace(r.namespace(ctx)).Update(ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.UpdateOptions{})
	return err
}

func (r *k8sIdentityRepository) DeleteUser(ctx context.Context, name string) error {
	return r.client.Resource(r.userGVR).Namespace(r.namespace(ctx)).Delete(ctx, name, metav1.DeleteOptions{})
}

// --- Groups ---

func (r *k8sIdentityRepository) ListGroups(ctx context.Context) ([]models.Group, error) {
	list, err := r.client.Resource(r.groupGVR).Namespace(r.namespace(ctx)).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var groups []models.Group
	for _, item := range list.Items {
		var crdObj crd.Group
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &crdObj); err != nil {
			logrus.WithError(err).Warn("Failed to convert group from unstructured")
			continue
		}
		groups = append(groups, models.Group{
			Name:    item.GetName(),
			Comment: crdObj.Spec.Comment,
		})
	}
	return groups, nil
}

func (r *k8sIdentityRepository) GetGroup(ctx context.Context, name string) (*models.Group, error) {
	u, err := r.client.Resource(r.groupGVR).Namespace(r.namespace(ctx)).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var crdObj crd.Group
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &crdObj); err != nil {
		return nil, fmt.Errorf("failed to convert from unstructured: %w", err)
	}

	return &models.Group{
		Name:        u.GetName(),
		Comment:     crdObj.Spec.Comment,
		Description: crdObj.Spec.Comment,
	}, nil
}

func (r *k8sIdentityRepository) CreateGroup(ctx context.Context, group *crd.Group) error {
	group.TypeMeta = metav1.TypeMeta{
		APIVersion: "kubauth.kubotal.io/v1alpha1",
		Kind:       "Group",
	}

	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(group)
	if err != nil {
		return fmt.Errorf("failed to convert to unstructured: %w", err)
	}

	_, err = r.client.Resource(r.groupGVR).Namespace(r.namespace(ctx)).Create(ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.CreateOptions{})
	return err
}

func (r *k8sIdentityRepository) UpdateGroup(ctx context.Context, group *crd.Group) error {
	current, err := r.client.Resource(r.groupGVR).Namespace(r.namespace(ctx)).Get(ctx, group.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	group.SetResourceVersion(current.GetResourceVersion())
	group.TypeMeta = metav1.TypeMeta{
		APIVersion: "kubauth.kubotal.io/v1alpha1",
		Kind:       "Group",
	}

	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(group)
	if err != nil {
		return fmt.Errorf("failed to convert to unstructured: %w", err)
	}

	_, err = r.client.Resource(r.groupGVR).Namespace(r.namespace(ctx)).Update(ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.UpdateOptions{})
	return err
}

func (r *k8sIdentityRepository) DeleteGroup(ctx context.Context, name string) error {
	return r.client.Resource(r.groupGVR).Namespace(r.namespace(ctx)).Delete(ctx, name, metav1.DeleteOptions{})
}

// --- GroupBindings ---

func (r *k8sIdentityRepository) ListGroupBindings(ctx context.Context, userFilter string) ([]models.GroupBinding, error) {
	// If userFilter is provided, we might want to use a FieldSelector if supported by the CRD/Server,
	// checking spec.user. But simple CRDs usually don't support arbitrary field selectors without indexing.
	// So we'll list all and filter in memory.

	list, err := r.client.Resource(r.gbGVR).Namespace(r.namespace(ctx)).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var bindings []models.GroupBinding
	for _, item := range list.Items {
		var crdObj crd.GroupBinding
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &crdObj); err != nil {
			logrus.WithError(err).Warn("Failed to convert group binding from unstructured")
			continue
		}

		if userFilter != "" && crdObj.Spec.User != userFilter {
			continue
		}

		bindings = append(bindings, models.GroupBinding{
			User:  crdObj.Spec.User,
			Group: crdObj.Spec.Group,
		})
	}
	return bindings, nil
}

func (r *k8sIdentityRepository) CreateGroupBinding(ctx context.Context, binding *crd.GroupBinding) error {
	binding.TypeMeta = metav1.TypeMeta{
		APIVersion: "kubauth.kubotal.io/v1alpha1",
		Kind:       "GroupBinding",
	}
	// Generate deterministic name if not provided: user-group-binding
	if binding.Name == "" {
		binding.Name = fmt.Sprintf("%s-%s", strings.ToLower(binding.Spec.User), strings.ToLower(binding.Spec.Group))
	}

	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(binding)
	if err != nil {
		return fmt.Errorf("failed to convert to unstructured: %w", err)
	}

	_, err = r.client.Resource(r.gbGVR).Namespace(r.namespace(ctx)).Create(ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.CreateOptions{})
	return err
}

func (r *k8sIdentityRepository) DeleteGroupBinding(ctx context.Context, name string) error {
	return r.client.Resource(r.gbGVR).Namespace(r.namespace(ctx)).Delete(ctx, name, metav1.DeleteOptions{})
}

func (r *k8sIdentityRepository) DeleteGroupBindingByRef(ctx context.Context, user, group string) error {
	// Need to find the binding first
	list, err := r.client.Resource(r.gbGVR).Namespace(r.namespace(ctx)).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, item := range list.Items {
		var crdObj crd.GroupBinding
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &crdObj); err != nil {
			continue
		}
		if crdObj.Spec.User == user && crdObj.Spec.Group == group {
			// Found it
			return r.DeleteGroupBinding(ctx, item.GetName())
		}
	}

	return fmt.Errorf("group binding not found for user %s and group %s", user, group)
}
