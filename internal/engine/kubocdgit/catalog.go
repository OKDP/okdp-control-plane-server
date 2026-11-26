package kubocdgit

import (
	"context"
	"errors"
	"fmt"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocd"
	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

var catalogPath = []string{"spec", "context", "serviceCatalog"}

// controlPlaneContext reads the control plane Context from the cached
// snapshot, nil when the file does not exist.
func (a *Adapter) controlPlaneContext(ctx context.Context) (*unstructured.Unstructured, error) {
	snap, err := a.snapshots.current(ctx)
	if err != nil {
		return nil, err
	}
	u, _, err := a.readControlPlaneContext(snap)
	return u, err
}

func (a *Adapter) readControlPlaneContext(snap *gitstore.Snapshot) (*unstructured.Unstructured, []byte, error) {
	p, err := gitstore.ControlPlaneContextFile(a.controlPlane.Name)
	if err != nil {
		return nil, nil, err
	}
	data, err := snap.ReadFile(p)
	if errors.Is(err, gitstore.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	obj, err := decode(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", p, err)
	}
	return &unstructured.Unstructured{Object: obj}, data, nil
}

// GetPlatformServices reads the catalog from the control plane Context in
// git, falling back to the platform Context while the file is absent or has
// no serviceCatalog.
func (a *Adapter) GetPlatformServices(ctx context.Context) ([]models.PlatformService, error) {
	u, err := a.controlPlaneContext(ctx)
	if err != nil {
		return nil, err
	}
	if u == nil || !repository.HasServiceCatalog(u) {
		return a.platform.GetPlatformServices(ctx)
	}
	return repository.ContextPlatformServices(u)
}

// GetMenuCategories reads the catalog sections like GetPlatformServices.
func (a *Adapter) GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error) {
	u, err := a.controlPlaneContext(ctx)
	if err != nil {
		return nil, err
	}
	if u == nil || !repository.HasServiceCatalog(u) {
		return a.platform.GetMenuCategories(ctx)
	}
	return repository.ContextMenuCategories(u)
}

// GetPackageRepository reads the default package repository like GetPlatformServices.
func (a *Adapter) GetPackageRepository(ctx context.Context) (string, error) {
	u, err := a.controlPlaneContext(ctx)
	if err != nil {
		return "", err
	}
	if u == nil || !repository.HasServiceCatalog(u) {
		return a.platform.GetPackageRepository(ctx)
	}
	return repository.ContextPackageRepository(u)
}

func (a *Adapter) catalogEntry(ctx context.Context, name string) (*models.PlatformService, error) {
	services, err := a.GetPlatformServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read platform services: %w", err)
	}
	for i := range services {
		if services[i].Name == name {
			return &services[i], nil
		}
	}
	return nil, fmt.Errorf("service %q is not available in the platform", name)
}

// --- engine.Catalog ---

func (a *Adapter) Entries(ctx context.Context) ([]models.PlatformService, error) {
	return a.GetPlatformServices(ctx)
}

func (a *Adapter) Services(ctx context.Context) ([]models.PlatformService, error) {
	services, err := a.GetPlatformServices(ctx)
	if err != nil {
		return nil, err
	}
	if a.packages == nil {
		return services, nil
	}
	versions := a.packages.ListVersionsForServices(ctx, services)
	for i, svc := range services {
		if tags, ok := versions[svc.Name]; ok {
			services[i].Versions = tags
		}
	}
	return services, nil
}

func (a *Adapter) Categories(ctx context.Context) ([]models.MenuCategory, error) {
	return a.GetMenuCategories(ctx)
}

func (a *Adapter) Versions(ctx context.Context, service string) (*models.ServiceVersions, error) {
	return a.packages.GetServiceVersions(ctx, service)
}

func (a *Adapter) ParameterSchema(ctx context.Context, service, version string) (map[string]any, error) {
	return a.packages.GetParameterSchema(ctx, service, version)
}

func (a *Adapter) Inputs(ctx context.Context, service, version string) ([]models.PackageInput, error) {
	return a.packages.GetPackageInputs(ctx, service, version)
}

// --- engine.PlatformConfig, read from the platform Context ---

func (a *Adapter) IngressSuffix(ctx context.Context) (string, error) {
	return a.platform.GetIngressSuffix(ctx)
}

func (a *Adapter) ProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error) {
	return a.platform.GetProfileImages(ctx)
}

func (a *Adapter) SparkConfig(ctx context.Context) (*models.SparkConfig, error) {
	return a.platform.GetSparkConfig(ctx)
}

// --- catalog writes ---

func (a *Adapter) CreateCatalogEntry(ctx context.Context, svc models.PlatformService, _ engine.WriteOptions) (engine.Operation, error) {
	if problem := kubocd.UnpublishedVersions(ctx, a.packages, svc); problem != "" {
		return engine.Rejected(engine.CodeInvalidCatalogEntry, problem), nil
	}
	return a.mutateCatalog(ctx, "Add "+svc.Name+" to the service catalog", func(categories []any) ([]any, error) {
		return repository.CatalogAdd(categories, svc)
	})
}

func (a *Adapter) UpdateCatalogEntry(ctx context.Context, svc models.PlatformService, _ engine.WriteOptions) (engine.Operation, error) {
	if problem := kubocd.UnpublishedVersions(ctx, a.packages, svc); problem != "" {
		return engine.Rejected(engine.CodeInvalidCatalogEntry, problem), nil
	}
	return a.mutateCatalog(ctx, "Update "+svc.Name+" in the service catalog", func(categories []any) ([]any, error) {
		return repository.CatalogUpdate(categories, svc.Name, svc)
	})
}

func (a *Adapter) DeleteCatalogEntry(ctx context.Context, name string, _ engine.WriteOptions) (engine.Operation, error) {
	return a.mutateCatalog(ctx, "Remove "+name+" from the service catalog", func(categories []any) ([]any, error) {
		return repository.CatalogRemove(categories, name)
	})
}

// mutateCatalog rewrites serviceCatalog.categories of the control plane
// Context, keeping every other field. When the file is missing or lacks a
// serviceCatalog section, it is seeded from the Kubernetes catalog first.
func (a *Adapter) mutateCatalog(ctx context.Context, message string, fn func([]any) ([]any, error)) (engine.Operation, error) {
	p, err := gitstore.ControlPlaneContextFile(a.controlPlane.Name)
	if err != nil {
		return engine.Operation{}, err
	}
	for attempt := 0; ; attempt++ {
		snap, err := a.snapshots.latest(ctx)
		if err != nil {
			return engine.Operation{}, err
		}
		u, data, err := a.readControlPlaneContext(snap)
		if err != nil {
			return engine.Operation{}, err
		}
		change := gitstore.Change{Path: p}
		var seededCatalog map[string]any
		if u == nil {
			catalog, err := a.seedCatalog(ctx)
			if err != nil {
				return engine.Operation{}, err
			}
			if data, err = render(a.contextManifest(catalog)); err != nil {
				return engine.Operation{}, err
			}
			if u, err = decodeUnstructured(data); err != nil {
				return engine.Operation{}, err
			}
			change.MustNotExist = true
		} else {
			if change.ExpectedRevision, err = a.snapshots.fileRevision(snap, p); err != nil {
				return engine.Operation{}, err
			}
			if !repository.HasServiceCatalog(u) {
				catalog, err := a.seedCatalog(ctx)
				if err != nil {
					return engine.Operation{}, err
				}
				if catalog == nil {
					catalog = map[string]any{"categories": []any{}}
				}
				seededCatalog = catalog
				if err := unstructured.SetNestedMap(u.Object, catalog, catalogPath...); err != nil {
					return engine.Operation{}, fmt.Errorf("failed to seed serviceCatalog: %w", err)
				}
			}
		}

		categories, _, err := unstructured.NestedSlice(u.Object, append(catalogPath, "categories")...)
		if err != nil {
			return engine.Operation{}, fmt.Errorf("failed to read serviceCatalog.categories: %w", err)
		}
		updatedCategories, err := fn(categories)
		if err != nil {
			switch {
			case errors.Is(err, repository.ErrCatalogEntryExists):
				return engine.Rejected(engine.CodeConflict, err.Error()), nil
			case errors.Is(err, repository.ErrCatalogEntryNotFound):
				return engine.Operation{}, notFound("%s", err.Error())
			default:
				return engine.Operation{}, err
			}
		}
		doc, err := parseDocument(data)
		if err != nil {
			return engine.Operation{}, fmt.Errorf("%s: %w", p, err)
		}
		if seededCatalog != nil {
			if err := doc.set(seededCatalog, catalogPath...); err != nil {
				return engine.Operation{}, err
			}
		}
		if err := doc.set(updatedCategories, append(catalogPath, "categories")...); err != nil {
			return engine.Operation{}, err
		}
		if change.Content, err = doc.bytes(); err != nil {
			return engine.Operation{}, err
		}
		res, err := a.commit(ctx, gitstore.WriteRequest{Changes: []gitstore.Change{change}, Message: message})
		if err != nil {
			if (errors.Is(err, gitstore.ErrConflict) || errors.Is(err, gitstore.ErrAlreadyExists)) && attempt < maxCASAttempts {
				continue
			}
			return failure(err, engine.CodeConflict)
		}
		a.platform.Invalidate()
		return engine.Operation{Outcome: engine.OutcomeAccepted, DesiredRevision: res.Files[p]}, nil
	}
}

func decodeUnstructured(data []byte) (*unstructured.Unstructured, error) {
	obj, err := decode(data)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: obj}, nil
}

// CatalogSource reads the serviceCatalog a new control plane Context starts
// from, false when Kubernetes has none. See repository.ReadServiceCatalog.
type CatalogSource func(ctx context.Context) (map[string]any, bool, error)

// seedCatalog is the serviceCatalog a missing control plane Context starts
// from: the one CatalogSource reads as it is, else the catalog the reads
// fell back to, rebuilt.
func (a *Adapter) seedCatalog(ctx context.Context) (map[string]any, error) {
	if a.catalogSource != nil {
		catalog, found, err := a.catalogSource(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to read the Kubernetes service catalog: %w", err)
		}
		if !found {
			return nil, nil
		}
		return catalog, nil
	}
	return a.rebuiltCatalog(ctx)
}

func (a *Adapter) rebuiltCatalog(ctx context.Context) (map[string]any, error) {
	services, err := a.platform.GetPlatformServices(ctx)
	if err != nil {
		return nil, err
	}
	categories := []any{}
	cats, err := a.platform.GetMenuCategories(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range cats {
		section := map[string]any{"title": c.Key, "services": []any{}}
		if c.Icon != "" {
			section["icon"] = c.Icon
		}
		categories = append(categories, section)
	}
	for _, svc := range services {
		if updated, err := repository.CatalogAdd(categories, svc); err == nil {
			categories = updated
		}
	}
	catalog := map[string]any{"categories": categories}
	if repo, err := a.platform.GetPackageRepository(ctx); err == nil {
		catalog["defaultRepository"] = repo
	}
	return catalog, nil
}

func (a *Adapter) contextManifest(catalog map[string]any) map[string]any {
	if catalog == nil {
		catalog = map[string]any{"categories": []any{}}
	}
	metadata := map[string]any{"name": a.controlPlane.Name}
	if a.controlPlane.Namespace != "" {
		metadata["namespace"] = a.controlPlane.Namespace
	}
	return map[string]any{
		"apiVersion": "kubocd.kubotal.io/v1alpha1",
		"kind":       "Context",
		"metadata":   metadata,
		"spec": map[string]any{
			"description": "OKDP control plane settings",
			"context":     map[string]any{"serviceCatalog": catalog},
		},
	}
}

// BootstrapControlPlaneContext commits contexts/<name>.yaml when the branch
// does not hold it yet, with the serviceCatalog of Kubernetes (Deps.Catalog)
// or an empty one. It never overwrites the file, and a concurrent bootstrap
// (another replica) is not an error. It reports whether it committed.
func (a *Adapter) BootstrapControlPlaneContext(ctx context.Context) (bool, error) {
	p, err := gitstore.ControlPlaneContextFile(a.controlPlane.Name)
	if err != nil {
		return false, err
	}
	snap, err := a.snapshots.latest(ctx)
	if err != nil {
		return false, err
	}
	if exists, err := snap.Exists(p); err != nil || exists {
		return false, err
	}
	var catalog map[string]any
	if a.catalogSource != nil {
		if catalog, err = a.seedCatalog(ctx); err != nil {
			return false, err
		}
	}
	content, err := render(a.contextManifest(catalog))
	if err != nil {
		return false, err
	}
	_, err = a.commit(ctx, gitstore.WriteRequest{
		Changes: []gitstore.Change{{Path: p, Content: content, MustNotExist: true}},
		Message: "Initialize the control plane Context " + a.controlPlane.Name,
	})
	if errors.Is(err, gitstore.ErrAlreadyExists) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	logrus.WithField("file", p).Info("Initialized the control plane Context in git")
	return true, nil
}

// ContextRepository is the platform Context reader with the catalog read
// from git, for the core services that read the Context directly.
func (a *Adapter) ContextRepository() repository.ContextRepository {
	return &gitContextRepository{ContextRepository: a.platform, adapter: a}
}

type gitContextRepository struct {
	repository.ContextRepository
	adapter *Adapter
}

func (r *gitContextRepository) Invalidate() {
	r.ContextRepository.Invalidate()
	r.adapter.snapshots.invalidate()
}

func (r *gitContextRepository) GetPlatformServices(ctx context.Context) ([]models.PlatformService, error) {
	return r.adapter.GetPlatformServices(ctx)
}

func (r *gitContextRepository) GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error) {
	return r.adapter.GetMenuCategories(ctx)
}

func (r *gitContextRepository) GetPackageRepository(ctx context.Context) (string, error) {
	return r.adapter.GetPackageRepository(ctx)
}
