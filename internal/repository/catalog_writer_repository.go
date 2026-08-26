package repository

import (
	"context"
	"fmt"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/sirupsen/logrus"
)

// CatalogWriterRepository manages the platform service catalog, the file
// platform/catalog.yaml of the deployments repository
// (categories[].services). A service is stored in the section whose title
// equals its Category, created on the fly when it does not exist yet.
type CatalogWriterRepository interface {
	// AddPlatformService appends a service to its section of the catalog.
	AddPlatformService(ctx context.Context, svc models.PlatformService) error
	// UpdatePlatformService replaces the service matching name, moving it when its Category changed.
	UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) error
	// RemovePlatformService drops the service matching name from its section.
	RemovePlatformService(ctx context.Context, name string) error
}

type gitCatalogWriterRepository struct {
	deployments *gitops.Deployments
}

// NewCatalogWriterRepository writes the catalog through the Git writer.
func NewCatalogWriterRepository(deployments *gitops.Deployments) CatalogWriterRepository {
	return &gitCatalogWriterRepository{deployments: deployments}
}

// mutateCategories performs a read-modify-write on the catalog's categories.
// The Git writer replays it on the latest revision when the branch moved, so
// concurrent edits don't clobber each other.
func (r *gitCatalogWriterRepository) mutateCategories(ctx context.Context, target string, fn func(categories []interface{}) ([]interface{}, error)) error {
	_, err := r.deployments.UpdateCatalog(ctx, auth.ActorName(ctx), target, func(doc map[string]interface{}) error {
		body := unwrapCatalog(doc)
		categories, _ := body["categories"].([]interface{})
		updated, err := fn(categories)
		if err != nil {
			return err
		}
		if updated == nil {
			updated = []interface{}{}
		}
		body["categories"] = updated
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to write %s: %w", gitops.CatalogPath, err)
	}
	return nil
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

func (r *gitCatalogWriterRepository) AddPlatformService(ctx context.Context, svc models.PlatformService) error {
	err := r.mutateCategories(ctx, svc.Name, func(categories []interface{}) ([]interface{}, error) {
		return appendServiceToCategory(categories, svc.Category, platformServiceToMap(svc)), nil
	})
	if err == nil {
		logrus.WithField("service", svc.Name).WithField("category", svc.Category).Info("Added platform service to catalog")
	}
	return err
}

func (r *gitCatalogWriterRepository) UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) error {
	err := r.mutateCategories(ctx, name, func(categories []interface{}) ([]interface{}, error) {
		if !removeServiceFromCategories(categories, name) {
			return categories, nil
		}
		return appendServiceToCategory(categories, svc.Category, platformServiceToMap(svc)), nil
	})
	if err == nil {
		logrus.WithField("service", name).Info("Updated platform service in catalog")
	}
	return err
}

func (r *gitCatalogWriterRepository) RemovePlatformService(ctx context.Context, name string) error {
	err := r.mutateCategories(ctx, name, func(categories []interface{}) ([]interface{}, error) {
		removeServiceFromCategories(categories, name)
		return categories, nil
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
