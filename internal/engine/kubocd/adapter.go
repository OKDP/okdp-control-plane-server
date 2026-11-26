// Package kubocd manages service instances as KuboCD Releases in Kubernetes.
// It accesses the service catalog and platform settings through the supplied
// Context repositories, while the KuboCD controller turns each Release into
// Flux HelmReleases.
package kubocd

import (
	"context"

	"k8s.io/client-go/dynamic"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

// Name is the engine type of this adapter.
const Name = "kubocd"

// PackageSource reads KuboCD packages from their OCI registry.
type PackageSource interface {
	GetParameterSchema(ctx context.Context, serviceName, tag string) (map[string]any, error)
	GetPackageInputs(ctx context.Context, serviceName, tag string) ([]models.PackageInput, error)
	GetServiceVersions(ctx context.Context, serviceName string) (*models.ServiceVersions, error)
	ListVersionsForServices(ctx context.Context, services []models.PlatformService) map[string][]string
	// ListPackageTags lists the published tags of a package, catalogued or not.
	// repositoryOverride wins over the Context's package repository when set.
	ListPackageTags(ctx context.Context, serviceName, repositoryOverride string) ([]string, error)
}

// Deps are what the adapter reads and writes through.
type Deps struct {
	Releases      repository.ServiceRepository
	Context       repository.ContextRepository
	ContextWriter repository.ContextWriterRepository
	Packages      PackageSource
	// Cluster lists the HelmReleases a Release owns.
	Cluster dynamic.Interface
	Config  Config
	// InsecureRegistries lists registry hosts reached over plain HTTP.
	InsecureRegistries []string
}

// Adapter implements the four engine ports on top of KuboCD.
type Adapter struct {
	releases      repository.ServiceRepository
	contextRepo   repository.ContextRepository
	contextWriter repository.ContextWriterRepository
	packages      PackageSource
	cluster       dynamic.Interface
	config        Config
	insecure      []string
}

// New returns the KuboCD engine.
func New(deps Deps) engine.Engine {
	a := &Adapter{
		releases:      deps.Releases,
		contextRepo:   deps.Context,
		contextWriter: deps.ContextWriter,
		packages:      deps.Packages,
		cluster:       deps.Cluster,
		config:        deps.Config,
		insecure:      deps.InsecureRegistries,
	}
	return engine.Engine{
		Info: engine.Info{
			Name:        Name,
			Persistence: engine.Persistence{Mode: engine.PersistenceKubernetes},
		},
		DesiredState:   a,
		Observer:       a,
		Catalog:        a,
		PlatformConfig: a,
	}
}

// ReleaseName is the name of the Release holding an instance.
func ReleaseName(ref engine.InstanceRef) string {
	return ref.Project + "-" + ref.Name
}
