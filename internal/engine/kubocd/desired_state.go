package kubocd

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

// notFoundError keeps the Kubernetes error and its message, and also matches
// engine.ErrNotFound.
type notFoundError struct{ err error }

func (e *notFoundError) Error() string   { return e.err.Error() }
func (e *notFoundError) Unwrap() []error { return []error{engine.ErrNotFound, e.err} }

// classify marks a Kubernetes NotFound error as engine.ErrNotFound and leaves
// any other error as it is.
func classify(err error) error {
	if err != nil && apierrors.IsNotFound(err) {
		return &notFoundError{err: err}
	}
	return err
}

// failure classifies a failed write. A write the API server refused (invalid,
// malformed, or forbidden by RBAC or an admission webhook) is rejected, a
// write that may have landed is unknown, anything else is an error. The
// Kubernetes message is kept as is.
func failure(err error) (engine.Operation, error) {
	switch {
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err), apierrors.IsForbidden(err):
		return engine.Rejected(engine.CodeRejected, err.Error()), nil
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), errors.Is(err, context.DeadlineExceeded):
		return engine.Operation{Outcome: engine.OutcomeUnknown, Message: err.Error()}, nil
	}
	return engine.Operation{}, classify(err)
}

// revision is the desired revision of a Release: its metadata.generation,
// empty when the API server has not set one.
func revision(release *crd.Release) string {
	if release == nil || release.Generation == 0 {
		return ""
	}
	return strconv.FormatInt(release.Generation, 10)
}

func (a *Adapter) catalogEntry(ctx context.Context, name string) (*models.PlatformService, error) {
	services, err := a.contextRepo.GetPlatformServices(ctx)
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

// CreateInstance creates the Release. WriteOptions.IdempotencyKey is ignored:
// the Release name already makes a replayed create fail as instance-exists.
func (a *Adapter) CreateInstance(ctx context.Context, in engine.Instance, _ engine.WriteOptions) (engine.Operation, error) {
	svc, err := a.catalogEntry(ctx, in.Service)
	if err != nil {
		return engine.Operation{}, err
	}
	packageRepo, err := a.contextRepo.GetPackageRepository(ctx)
	if err != nil {
		return engine.Operation{}, fmt.Errorf("failed to resolve package repository: %w", err)
	}
	if svc.Repository != "" {
		packageRepo = svc.Repository
	}

	project := in.Ref.Project
	release := &crd.Release{
		TypeMeta: metav1.TypeMeta{
			APIVersion: crd.ReleaseAPIVersion,
			Kind:       crd.ReleaseKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ReleaseName(in.Ref),
			Namespace: project,
			Labels: map[string]string{
				crd.LabelProject:      project,
				crd.LabelService:      in.Service,
				crd.LabelInstanceName: in.Ref.Name,
			},
		},
		Spec: crd.ReleaseSpec{
			Description: fmt.Sprintf("%s for project %s", in.Service, project),
			Package: crd.ReleasePackage{
				Repository: fmt.Sprintf("%s/%s", packageRepo, in.Service),
				Tag:        in.Version,
				Interval:   a.config.ReleaseInterval,
				Timeout:    a.config.ReleaseTimeout,
				Insecure:   repository.InsecureOCIHost(packageRepo, a.insecure),
			},
			Parameters: in.Parameters,
			// No explicit Contexts. KuboCD merges Config.defaultContexts, then the
			// optional Context named by Config.defaultNamespaceContexts looked up in
			// this Release's namespace. A project overriding nothing needs no object.
			TargetNamespace: project,
			CreateNamespace: false,
		},
	}

	created, err := a.releases.Create(ctx, project, release)
	if apierrors.IsAlreadyExists(err) {
		return engine.Rejected(engine.CodeInstanceExists, err.Error()), nil
	}
	if err != nil {
		return failure(err)
	}

	// The answer describes the Release as submitted, without the metadata the
	// API server assigns.
	view := ReleaseView(release)
	view.Status.DesiredRevision = revision(created)
	return engine.Operation{Outcome: engine.OutcomeAccepted, DesiredRevision: view.Status.DesiredRevision, Instance: &view}, nil
}

const maxResourceVersionConflicts = 5

// UpdateInstance sets the version and replaces the parameters of the Release.
// With an ExpectedRevision, the write is refused as a conflict if the Release
// generation changed since. A status/metadata update that bumps resourceVersion
// without changing metadata.generation is retried up to a bounded number of times.
func (a *Adapter) UpdateInstance(ctx context.Context, in engine.Instance, opts engine.WriteOptions) (engine.Operation, error) {
	project := in.Ref.Project
	for attempt := 0; ; attempt++ {
		release, err := a.releases.Get(ctx, project, ReleaseName(in.Ref))
		if err != nil {
			return engine.Operation{}, classify(err)
		}
		if opts.ExpectedRevision != "" && opts.ExpectedRevision != revision(release) {
			return conflict(in.Ref, opts.ExpectedRevision), nil
		}

		release.Spec.Package.Tag = in.Version
		release.Spec.Parameters = in.Parameters

		var updated *crd.Release
		if opts.ExpectedRevision != "" {
			updated, err = a.releases.UpdateIfUnchanged(ctx, project, release)
			if apierrors.IsConflict(err) {
				if attempt+1 < maxResourceVersionConflicts {
					continue
				}
				return conflict(in.Ref, opts.ExpectedRevision), nil
			}
		} else {
			updated, err = a.releases.Update(ctx, project, release)
		}
		if err != nil {
			return failure(fmt.Errorf("failed to update release: %w", err))
		}

		views := []engine.InstanceView{ReleaseView(updated)}
		a.withConnectionStates(ctx, project, views, nil)
		view := views[0]
		view.Status.DesiredRevision = revision(updated)
		return engine.Operation{Outcome: engine.OutcomeAccepted, DesiredRevision: view.Status.DesiredRevision, Instance: &view}, nil
	}
}

func conflict(ref engine.InstanceRef, expected string) engine.Operation {
	return engine.Rejected(engine.CodeConflict,
		fmt.Sprintf("instance %q changed since revision %s", ref.Name, expected))
}

// DeleteInstance deletes the Release. With an ExpectedRevision, the deletion
// is refused as a conflict if the Release generation changed since. A
// resourceVersion conflict caused by a controller status update without a
// generation bump is retried.
func (a *Adapter) DeleteInstance(ctx context.Context, ref engine.InstanceRef, opts engine.WriteOptions) (engine.Operation, error) {
	name := ReleaseName(ref)
	if opts.ExpectedRevision != "" {
		for attempt := 0; ; attempt++ {
			release, getErr := a.releases.Get(ctx, ref.Project, name)
			if getErr != nil {
				return engine.Operation{}, classify(getErr)
			}
			if opts.ExpectedRevision != revision(release) {
				return conflict(ref, opts.ExpectedRevision), nil
			}
			err := a.releases.DeleteIfUnchanged(ctx, ref.Project, name, release.ResourceVersion)
			if apierrors.IsConflict(err) {
				if attempt+1 < maxResourceVersionConflicts {
					continue
				}
				return conflict(ref, opts.ExpectedRevision), nil
			}
			if err != nil {
				return failure(err)
			}
			break
		}
	} else {
		if err := a.releases.Delete(ctx, ref.Project, name); err != nil {
			return failure(err)
		}
	}
	return engine.Operation{
		Outcome:  engine.OutcomeAccepted,
		Instance: &engine.InstanceView{Ref: ref, EngineName: name, TargetNamespace: ref.Project},
	}, nil
}

func (a *Adapter) GetInstance(ctx context.Context, ref engine.InstanceRef) (engine.Instance, string, error) {
	release, err := a.releases.Get(ctx, ref.Project, ReleaseName(ref))
	if err != nil {
		return engine.Instance{}, "", classify(err)
	}
	in := desired(release)
	in.Ref = ref
	return in, revision(release), nil
}

func (a *Adapter) ListInstances(ctx context.Context, project string) ([]engine.Instance, error) {
	releases, err := a.releases.List(ctx, project, project)
	if err != nil {
		return nil, err
	}
	out := make([]engine.Instance, 0, len(releases))
	for i := range releases {
		out = append(out, desired(&releases[i]))
	}
	return out, nil
}

func desired(release *crd.Release) engine.Instance {
	view := ReleaseView(release)
	return engine.Instance{Ref: view.Ref, Service: view.Service, Version: view.Version, Parameters: view.Parameters}
}

// --- Catalog entries, stored through the configured Context writer ---

var errCatalogUnavailable = errors.New("catalog management is not available")

func (a *Adapter) CreateCatalogEntry(ctx context.Context, svc models.PlatformService, _ engine.WriteOptions) (engine.Operation, error) {
	if a.contextWriter == nil {
		return engine.Operation{}, errCatalogUnavailable
	}
	if problem := UnpublishedVersions(ctx, a.packages, svc); problem != "" {
		return engine.Rejected(engine.CodeInvalidCatalogEntry, problem), nil
	}
	if err := a.contextWriter.AddPlatformService(ctx, svc); err != nil {
		if errors.Is(err, repository.ErrCatalogEntryExists) {
			return engine.Rejected(engine.CodeConflict, err.Error()), nil
		}
		return failure(err)
	}
	a.contextRepo.Invalidate()
	return engine.Operation{Outcome: engine.OutcomeAccepted}, nil
}

func (a *Adapter) UpdateCatalogEntry(ctx context.Context, svc models.PlatformService, _ engine.WriteOptions) (engine.Operation, error) {
	if a.contextWriter == nil {
		return engine.Operation{}, errCatalogUnavailable
	}
	if problem := UnpublishedVersions(ctx, a.packages, svc); problem != "" {
		return engine.Rejected(engine.CodeInvalidCatalogEntry, problem), nil
	}
	if err := a.contextWriter.UpdatePlatformService(ctx, svc.Name, svc); err != nil {
		if errors.Is(err, repository.ErrCatalogEntryNotFound) {
			return engine.Operation{}, &notFoundError{err: err}
		}
		return failure(err)
	}
	a.contextRepo.Invalidate()
	return engine.Operation{Outcome: engine.OutcomeAccepted}, nil
}

func (a *Adapter) DeleteCatalogEntry(ctx context.Context, name string, _ engine.WriteOptions) (engine.Operation, error) {
	if a.contextWriter == nil {
		return engine.Operation{}, errCatalogUnavailable
	}
	if err := a.contextWriter.RemovePlatformService(ctx, name); err != nil {
		if errors.Is(err, repository.ErrCatalogEntryNotFound) {
			return engine.Operation{}, &notFoundError{err: err}
		}
		return failure(err)
	}
	a.contextRepo.Invalidate()
	return engine.Operation{Outcome: engine.OutcomeAccepted}, nil
}

// UnpublishedVersions describes the versions of svc missing from its package
// registry, or a registry that cannot be queried. Empty when all are
// published or when packages is nil.
func UnpublishedVersions(ctx context.Context, packages PackageSource, svc models.PlatformService) string {
	if packages == nil {
		return ""
	}
	tags, err := packages.ListPackageTags(ctx, svc.Name, svc.Repository)
	if err != nil {
		return fmt.Sprintf("could not verify package %q in the registry (%v)", svc.Name, err)
	}
	available := make(map[string]bool, len(tags))
	for _, t := range tags {
		available[t] = true
	}
	var missing []string
	for _, v := range svc.Versions {
		if !available[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("version(s) %v not found in the registry for %q (available: %v)", missing, svc.Name, tags)
	}
	return ""
}
