package repository

import (
	"context"
	"fmt"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
)

// ContextWriterRepository manages the platform service catalog on the platform
// Context (spec.context.serviceCatalog.services).
//
// Per-project configuration is not its business: KuboCD resolves an optional
// Context by name in the namespace of each Release, through
// Config.defaultNamespaceContexts.
type ContextWriterRepository interface {
	// AddPlatformService appends a service to the default Context's serviceCatalog.services.
	AddPlatformService(ctx context.Context, svc models.PlatformService) error
	// UpdatePlatformService replaces the service matching name in serviceCatalog.services.
	UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) error
	// RemovePlatformService drops the service matching name from serviceCatalog.services.
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

// mutateServices performs a read-modify-write on the default Context's serviceCatalog.services
// list, retrying on resource-version conflicts so concurrent edits don't clobber each other.
func (r *k8sContextWriterRepository) mutateServices(ctx context.Context, fn func(services []interface{}) ([]interface{}, error)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Get(ctx, r.defaultName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to read default context %s/%s: %w", r.defaultNamespace, r.defaultName, err)
		}

		services, _, err := unstructured.NestedSlice(cur.Object, "spec", "context", "serviceCatalog", "services")
		if err != nil {
			return fmt.Errorf("failed to read serviceCatalog.services: %w", err)
		}

		updated, err := fn(services)
		if err != nil {
			return err
		}

		if err := unstructured.SetNestedSlice(cur.Object, updated, "spec", "context", "serviceCatalog", "services"); err != nil {
			return fmt.Errorf("failed to set serviceCatalog.services: %w", err)
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
// under spec.context.serviceCatalog.services (versions as []interface{} of strings).
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
