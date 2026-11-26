package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/sirupsen/logrus"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
)

// Catalog CAS errors returned when a catalog mutation races with another writer.
var (
	ErrCatalogEntryExists   = errors.New("service already exists in the catalog")
	ErrCatalogEntryNotFound = errors.New("service not found in the catalog")
)

// ContextWriterRepository manages the service catalog on the Control Plane
// Context (spec.context.serviceCatalog.categories[].services). A service is
// stored in the section whose title equals its Category, created on the fly
// when it does not exist yet.
//
// Per-project configuration is not its business: KuboCD resolves an optional
// Context by name in the namespace of each Release, through
// Config.defaultNamespaceContexts.
type ContextWriterRepository interface {
	// AddPlatformService appends a service to its section of the catalog.
	AddPlatformService(ctx context.Context, svc models.PlatformService) error
	// UpdatePlatformService replaces the service matching name, moving it when its Category changed.
	UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) error
	// RemovePlatformService drops the service matching name from its section.
	RemovePlatformService(ctx context.Context, name string) error
}

type k8sContextWriterRepository struct {
	client       dynamic.Interface
	platform     ContextRef
	controlPlane ContextRef
}

// NewContextWriterRepository writes the catalog of one Context.
func NewContextWriterRepository(client dynamic.Interface, defaultName, defaultNamespace string) ContextWriterRepository {
	ref := ContextRef{Name: defaultName, Namespace: defaultNamespace}
	return NewSplitContextWriterRepository(client, ref, ref)
}

// NewSplitContextWriterRepository writes the catalog of controlPlane and
// never writes platform, unless both name the same Context. A Control Plane
// Context without serviceCatalog starts from a copy of the platform one, the
// catalog the reads fell back to, and a missing one is created that way.
func NewSplitContextWriterRepository(client dynamic.Interface, platform, controlPlane ContextRef) ContextWriterRepository {
	return &k8sContextWriterRepository{client: client, platform: platform, controlPlane: controlPlane}
}

// mutateCategories performs a read-modify-write on the Control Plane
// Context's serviceCatalog.categories list, retrying on resource-version
// conflicts so concurrent edits don't clobber each other.
func (r *k8sContextWriterRepository) mutateCategories(ctx context.Context, fn func(categories []interface{}) ([]interface{}, error)) error {
	contexts := r.client.Resource(contextGVR)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := contexts.Namespace(r.controlPlane.Namespace).Get(ctx, r.controlPlane.Name, metav1.GetOptions{})
		missing := apierrors.IsNotFound(err) && r.controlPlane != r.platform
		switch {
		case missing:
			cur = &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": contextGVR.GroupVersion().String(),
				"kind":       "Context",
				"metadata":   map[string]interface{}{"name": r.controlPlane.Name, "namespace": r.controlPlane.Namespace},
				"spec":       map[string]interface{}{"context": map[string]interface{}{}},
			}}
		case err != nil:
			return fmt.Errorf("failed to read the control plane context %s: %w", r.controlPlane, err)
		}

		if r.controlPlane != r.platform && !HasServiceCatalog(cur) {
			platform, err := contexts.Namespace(r.platform.Namespace).Get(ctx, r.platform.Name, metav1.GetOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to read the platform context %s: %w", r.platform, err)
			}
			if err == nil {
				if catalog, found, _ := unstructured.NestedMap(platform.Object, "spec", "context", "serviceCatalog"); found {
					if err := unstructured.SetNestedMap(cur.Object, catalog, "spec", "context", "serviceCatalog"); err != nil {
						return fmt.Errorf("failed to seed serviceCatalog: %w", err)
					}
				}
			}
		}

		categories, _, err := unstructured.NestedSlice(cur.Object, "spec", "context", "serviceCatalog", "categories")
		if err != nil {
			return fmt.Errorf("failed to read serviceCatalog.categories: %w", err)
		}

		updated, err := fn(categories)
		if err != nil {
			return err
		}

		if err := unstructured.SetNestedSlice(cur.Object, updated, "spec", "context", "serviceCatalog", "categories"); err != nil {
			return fmt.Errorf("failed to set serviceCatalog.categories: %w", err)
		}

		if missing {
			_, err = contexts.Namespace(r.controlPlane.Namespace).Create(ctx, cur, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				// Created meanwhile: read it again.
				return apierrors.NewConflict(contextGVR.GroupResource(), r.controlPlane.Name, err)
			}
			return err
		}
		_, err = contexts.Namespace(r.controlPlane.Namespace).Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}

// categoryServices returns the services list of a section, always as a slice.
func categoryServices(cat map[string]interface{}) []interface{} {
	services, _ := cat["services"].([]interface{})
	return services
}

// removeServiceFromCategories drops every service named name from every
// section and reports whether one was found.
func removeServiceFromCategories(categories []interface{}, name string) bool {
	found := false
	for _, raw := range categories {
		cat, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		services := categoryServices(cat)
		kept := make([]interface{}, 0, len(services))
		for _, s := range services {
			if m, ok := s.(map[string]interface{}); ok && getString(m, "name") == name {
				found = true
				continue
			}
			kept = append(kept, s)
		}
		cat["services"] = kept
	}
	return found
}

// appendServiceToCategory adds the service under the section titled title,
// creating the section at the end of the list when it does not exist.
func appendServiceToCategory(categories []interface{}, title string, svc map[string]interface{}) []interface{} {
	for _, raw := range categories {
		cat, ok := raw.(map[string]interface{})
		if !ok || getString(cat, "title") != title {
			continue
		}
		cat["services"] = append(categoryServices(cat), svc)
		return categories
	}
	return append(categories, map[string]interface{}{
		"title":    title,
		"services": []interface{}{svc},
	})
}

// CatalogAdd adds svc to the catalog sections categories, returning
// ErrCatalogEntryExists when a service with the same name is already present.
func CatalogAdd(categories []interface{}, svc models.PlatformService) ([]interface{}, error) {
	if cat, _ := findService(categories, svc.Name); cat != nil {
		return nil, fmt.Errorf("%w: %q", ErrCatalogEntryExists, svc.Name)
	}
	return appendServiceToCategory(categories, svc.Category, platformServiceToMap(svc)), nil
}

// CatalogUpdate replaces the service matching name with svc, in place, or
// at the end of its new category when the category changed. Fields of the
// stored service that PlatformService does not know are kept. Returns
// ErrCatalogEntryNotFound when no service matches.
func CatalogUpdate(categories []interface{}, name string, svc models.PlatformService) ([]interface{}, error) {
	cat, index := findService(categories, name)
	if cat == nil {
		return nil, fmt.Errorf("%w: %q", ErrCatalogEntryNotFound, name)
	}
	services := categoryServices(cat)
	updated := platformServiceToMap(svc)
	for key, value := range services[index].(map[string]interface{}) {
		if _, known := knownServiceFields[key]; !known {
			updated[key] = value
		}
	}
	if getString(cat, "title") == svc.Category {
		services[index] = updated
		return categories, nil
	}
	removeServiceFromCategories(categories, name)
	return appendServiceToCategory(categories, svc.Category, updated), nil
}

// knownServiceFields are the fields platformServiceToMap writes or omits.
var knownServiceFields = map[string]struct{}{
	"name": {}, "versions": {}, "default": {}, "tag": {}, "description": {}, "repository": {}, "label": {}, "exposesUI": {},
}

// findService returns the category holding the service name and its index
// in the category's services, nil when no category holds it.
func findService(categories []interface{}, name string) (map[string]interface{}, int) {
	for _, raw := range categories {
		cat, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		for i, s := range categoryServices(cat) {
			if m, ok := s.(map[string]interface{}); ok && getString(m, "name") == name {
				return cat, i
			}
		}
	}
	return nil, -1
}

// CatalogRemove drops the services matching name, returning
// ErrCatalogEntryNotFound when no service matches.
func CatalogRemove(categories []interface{}, name string) ([]interface{}, error) {
	if !removeServiceFromCategories(categories, name) {
		return nil, fmt.Errorf("%w: %q", ErrCatalogEntryNotFound, name)
	}
	return categories, nil
}

func (r *k8sContextWriterRepository) AddPlatformService(ctx context.Context, svc models.PlatformService) error {
	err := r.mutateCategories(ctx, func(categories []interface{}) ([]interface{}, error) {
		return CatalogAdd(categories, svc)
	})
	if err == nil {
		logrus.WithField("service", svc.Name).WithField("category", svc.Category).Info("Added platform service to catalog")
	}
	return err
}

func (r *k8sContextWriterRepository) UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) error {
	err := r.mutateCategories(ctx, func(categories []interface{}) ([]interface{}, error) {
		return CatalogUpdate(categories, name, svc)
	})
	if err == nil {
		logrus.WithField("service", name).Info("Updated platform service in catalog")
	}
	return err
}

func (r *k8sContextWriterRepository) RemovePlatformService(ctx context.Context, name string) error {
	err := r.mutateCategories(ctx, func(categories []interface{}) ([]interface{}, error) {
		return CatalogRemove(categories, name)
	})
	if err == nil {
		logrus.WithField("service", name).Info("Removed platform service from catalog")
	}
	return err
}

// platformServiceToMap converts a PlatformService into the unstructured shape
// stored under a catalog section (versions as []interface{} of strings). The
// section carries the category, the service does not repeat it.
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
	if svc.Repository != "" {
		m["repository"] = svc.Repository
	}
	if svc.Label != "" {
		m["label"] = svc.Label
	}
	if svc.ExposesUI != nil {
		m["exposesUI"] = *svc.ExposesUI
	}
	return m
}
