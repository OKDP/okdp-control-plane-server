package repository

import (
	"context"
	"fmt"

	"github.com/okdp/okdp-server-new/internal/models"
	"github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
)

// ContextWriterRepository creates, syncs, and deletes KuboCD Context CRs for per-project isolation,
// and manages the platform service catalog on the default Context (spec.context.okdp.services).
type ContextWriterRepository interface {
	CreateFromDefault(ctx context.Context, projectName string) error
	SyncFromDefault(ctx context.Context, projectName string) error
	Delete(ctx context.Context, projectName string) error

	// AddPlatformService appends a service to the default Context's okdp.services.
	AddPlatformService(ctx context.Context, svc models.PlatformService) error
	// UpdatePlatformService replaces the service matching name in okdp.services.
	UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) error
	// RemovePlatformService drops the service matching name from okdp.services.
	RemovePlatformService(ctx context.Context, name string) error
}

type k8sContextWriterRepository struct {
	client           dynamic.Interface
	defaultName      string
	defaultNamespace string
}

func NewContextWriterRepository(client dynamic.Interface, defaultName, defaultNamespace string) ContextWriterRepository {
	return &k8sContextWriterRepository{
		client:           client,
		defaultName:      defaultName,
		defaultNamespace: defaultNamespace,
	}
}

// CreateFromDefault copies the default Context CR into a project-scoped Context.
func (r *k8sContextWriterRepository) CreateFromDefault(ctx context.Context, projectName string) error {
	defaultCtx, err := r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Get(ctx, r.defaultName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to read default context %s/%s: %w", r.defaultNamespace, r.defaultName, err)
	}

	specContext, found, err := unstructured.NestedMap(defaultCtx.Object, "spec", "context")
	if err != nil || !found {
		return fmt.Errorf("default context has no spec.context")
	}

	projectCtx := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kubocd.kubotal.io/v1alpha1",
			"kind":       "Context",
			"metadata": map[string]interface{}{
				"name":      projectName,
				"namespace": r.defaultNamespace,
				"labels": map[string]interface{}{
					"okdp.io/project": projectName,
					"okdp.io/source":  "default",
				},
			},
			"spec": map[string]interface{}{
				"context": specContext,
			},
		},
	}

	_, err = r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Create(ctx, projectCtx, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create project context %q: %w", projectName, err)
	}

	logrus.WithField("project", projectName).Info("Created per-project Context CR")
	return nil
}

// SyncFromDefault creates the project Context if missing, or updates it from the default if it already exists.
// This ensures project contexts always reflect the latest default context (e.g. new service blocks).
func (r *k8sContextWriterRepository) SyncFromDefault(ctx context.Context, projectName string) error {
	defaultCtx, err := r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Get(ctx, r.defaultName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to read default context %s/%s: %w", r.defaultNamespace, r.defaultName, err)
	}

	specContext, found, err := unstructured.NestedMap(defaultCtx.Object, "spec", "context")
	if err != nil || !found {
		return fmt.Errorf("default context has no spec.context")
	}

	existing, err := r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Get(ctx, projectName, metav1.GetOptions{})
	if err != nil {
		logrus.WithField("project", projectName).Info("Project Context missing, creating from default")
		return r.CreateFromDefault(ctx, projectName)
	}

	if err := unstructured.SetNestedMap(existing.Object, specContext, "spec", "context"); err != nil {
		return fmt.Errorf("failed to set spec.context on project context %q: %w", projectName, err)
	}

	_, err = r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Update(ctx, existing, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update project context %q: %w", projectName, err)
	}

	logrus.WithField("project", projectName).Info("Synced project Context from default")
	return nil
}

func (r *k8sContextWriterRepository) Delete(ctx context.Context, projectName string) error {
	err := r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Delete(ctx, projectName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete project context %q: %w", projectName, err)
	}
	logrus.WithField("project", projectName).Info("Deleted per-project Context CR")
	return nil
}

// --- Catalog management (spec.context.okdp.services on the default Context) ---

// mutateServices performs a read-modify-write on the default Context's okdp.services
// list, retrying on resource-version conflicts so concurrent edits don't clobber each other.
func (r *k8sContextWriterRepository) mutateServices(ctx context.Context, fn func(services []interface{}) ([]interface{}, error)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Get(ctx, r.defaultName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to read default context %s/%s: %w", r.defaultNamespace, r.defaultName, err)
		}

		services, _, err := unstructured.NestedSlice(cur.Object, "spec", "context", "okdp", "services")
		if err != nil {
			return fmt.Errorf("failed to read okdp.services: %w", err)
		}

		updated, err := fn(services)
		if err != nil {
			return err
		}

		if err := unstructured.SetNestedSlice(cur.Object, updated, "spec", "context", "okdp", "services"); err != nil {
			return fmt.Errorf("failed to set okdp.services: %w", err)
		}

		_, err = r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}

func (r *k8sContextWriterRepository) AddPlatformService(ctx context.Context, svc models.PlatformService) error {
	err := r.mutateServices(ctx, func(services []interface{}) ([]interface{}, error) {
		return append(services, platformServiceToMap(svc)), nil
	})
	if err == nil {
		logrus.WithField("service", svc.Name).Info("Added platform service to catalog")
	}
	return err
}

func (r *k8sContextWriterRepository) UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) error {
	err := r.mutateServices(ctx, func(services []interface{}) ([]interface{}, error) {
		for i, raw := range services {
			if m, ok := raw.(map[string]interface{}); ok && getString(m, "name") == name {
				services[i] = platformServiceToMap(svc)
				break
			}
		}
		return services, nil
	})
	if err == nil {
		logrus.WithField("service", name).Info("Updated platform service in catalog")
	}
	return err
}

func (r *k8sContextWriterRepository) RemovePlatformService(ctx context.Context, name string) error {
	err := r.mutateServices(ctx, func(services []interface{}) ([]interface{}, error) {
		filtered := make([]interface{}, 0, len(services))
		for _, raw := range services {
			if m, ok := raw.(map[string]interface{}); ok && getString(m, "name") == name {
				continue
			}
			filtered = append(filtered, raw)
		}
		return filtered, nil
	})
	if err == nil {
		logrus.WithField("service", name).Info("Removed platform service from catalog")
	}
	return err
}

// platformServiceToMap converts a PlatformService into the unstructured shape stored
// under spec.context.okdp.services (versions as []interface{} of strings).
func platformServiceToMap(svc models.PlatformService) map[string]interface{} {
	versions := make([]interface{}, 0, len(svc.Versions))
	for _, v := range svc.Versions {
		versions = append(versions, v)
	}
	m := map[string]interface{}{
		"name":     svc.Name,
		"versions": versions,
		"default":  svc.DefaultVersion,
	}
	if svc.Description != "" {
		m["description"] = svc.Description
	}
	if svc.Icon != "" {
		m["icon"] = svc.Icon
	}
	if svc.Category != "" {
		m["category"] = svc.Category
	}
	if svc.Repository != "" {
		m["repository"] = svc.Repository
	}
	return m
}
