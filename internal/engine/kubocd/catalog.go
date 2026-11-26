package kubocd

import (
	"context"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// Entries returns the services declared in the configured service catalog.
func (a *Adapter) Entries(ctx context.Context) ([]models.PlatformService, error) {
	return a.contextRepo.GetPlatformServices(ctx)
}

// Services returns the catalog services, each with the versions published in
// its package registry when the registry answers.
func (a *Adapter) Services(ctx context.Context) ([]models.PlatformService, error) {
	services, err := a.contextRepo.GetPlatformServices(ctx)
	if err != nil {
		return nil, err
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
	return a.contextRepo.GetMenuCategories(ctx)
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
