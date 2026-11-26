package kubocdgit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

// CreateInstance commits the Release. A replay with the same idempotency key
// answers the first commit, as long as the manifest renders the same: a
// change of the catalog repository in between makes it a conflict.
func (a *Adapter) CreateInstance(ctx context.Context, in engine.Instance, opts engine.WriteOptions) (engine.Operation, error) {
	// The instance name ends up in label values, 63 characters at most: a
	// longer one would make the sync tool refuse the whole branch.
	if errs := validation.IsDNS1123Label(in.Ref.Name); len(errs) > 0 {
		return engine.Rejected(engine.CodeRejected, fmt.Sprintf("instance name %q: %s", in.Ref.Name, strings.Join(errs, ", "))), nil
	}
	name := releaseName(in.Ref)
	p, err := gitstore.ReleaseFile(in.Ref.Project, name)
	if err != nil {
		return engine.Rejected(engine.CodeRejected, err.Error()), nil
	}
	svc, err := a.catalogEntry(ctx, in.Service)
	if err != nil {
		return engine.Operation{}, err
	}
	packageRepo := svc.Repository
	if packageRepo == "" {
		if packageRepo, err = a.GetPackageRepository(ctx); err != nil {
			return engine.Operation{}, fmt.Errorf("failed to resolve package repository: %w", err)
		}
	}

	manifest := a.releaseManifest(in, name, packageRepo)
	content, err := render(manifest)
	if err != nil {
		return engine.Operation{}, err
	}
	res, err := a.commit(ctx, gitstore.WriteRequest{
		Changes:        []gitstore.Change{{Path: p, Content: content, MustNotExist: true}},
		Message:        fmt.Sprintf("Create %s in project %s", in.Ref.Name, in.Ref.Project),
		IdempotencyKey: idempotencyKey(in.Ref, opts.IdempotencyKey),
	})
	if err != nil {
		return failure(err, engine.CodeInstanceExists)
	}
	f, err := parseRelease(p, content, in.Ref.Project)
	if err != nil {
		return engine.Operation{}, err
	}
	view := a.pendingView(f, res.Files[p])
	return engine.Operation{Outcome: engine.OutcomeAccepted, DesiredRevision: res.Files[p], Instance: &view}, nil
}

func (a *Adapter) releaseManifest(in engine.Instance, name, packageRepo string) map[string]any {
	pkg := map[string]any{
		"repository": fmt.Sprintf("%s/%s", packageRepo, in.Service),
		"tag":        in.Version,
	}
	if a.config.ReleaseInterval != "" {
		pkg["interval"] = a.config.ReleaseInterval
	}
	if a.config.ReleaseTimeout != "" {
		pkg["timeout"] = a.config.ReleaseTimeout
	}
	if repository.InsecureOCIHost(packageRepo, a.insecure) {
		pkg["insecure"] = true
	}
	spec := map[string]any{
		"description":     fmt.Sprintf("%s for project %s", in.Service, in.Ref.Project),
		"package":         pkg,
		"targetNamespace": in.Ref.Project,
		"createNamespace": false,
		// User-managed project services must remain deletable by Flux prune even
		// before KuboCD computes package protection or while a project namespace
		// is terminating (otherwise the deletion webhook blocks the Kustomization).
		"protected": false,
	}
	if len(in.Parameters) > 0 {
		spec["parameters"] = in.Parameters
	}
	return map[string]any{
		"apiVersion": crd.ReleaseAPIVersion,
		"kind":       crd.ReleaseKind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": in.Ref.Project,
			"labels": map[string]any{
				crd.LabelProject:      in.Ref.Project,
				crd.LabelService:      in.Service,
				crd.LabelInstanceName: in.Ref.Name,
			},
		},
		"spec": spec,
	}
}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// idempotencyKey scopes the caller's key to the instance, so that one key
// reused for another instance is a conflict and not a replay. A key the
// store would refuse is hashed.
func idempotencyKey(ref engine.InstanceRef, key string) string {
	if key == "" {
		return ""
	}
	scoped := fmt.Sprintf("instance:%s/%s:%s", ref.Project, ref.Name, key)
	if keyPattern.MatchString(scoped) {
		return scoped
	}
	sum := sha256.Sum256([]byte(scoped))
	return "instance:sha256-" + hex.EncodeToString(sum[:])
}

// UpdateInstance sets the version and replaces the parameters, keeping every
// other field of the file. Without an ExpectedRevision it still refuses to
// overwrite a change made since it read the file, and starts again from it.
func (a *Adapter) UpdateInstance(ctx context.Context, in engine.Instance, opts engine.WriteOptions) (engine.Operation, error) {
	if opts.ExpectedRevision != "" && !validRevision(opts.ExpectedRevision) {
		return conflict(in.Ref, opts.ExpectedRevision), nil
	}
	for attempt := 0; ; attempt++ {
		f, current, err := a.readRelease(ctx, in.Ref)
		if err != nil {
			if errors.Is(err, engine.ErrNotFound) && a.releaseOnlyInCluster(ctx, in.Ref) {
				return engine.Rejected(engine.CodeConflict, fmt.Sprintf("instance %q exists in the cluster but not in git", in.Ref.Name)), nil
			}
			return engine.Operation{}, err
		}
		if opts.ExpectedRevision != "" && opts.ExpectedRevision != current {
			return conflict(in.Ref, opts.ExpectedRevision), nil
		}
		doc, err := parseDocument(f.data)
		if err != nil {
			return engine.Operation{}, fmt.Errorf("%s: %w", f.path, err)
		}
		if err := doc.set(in.Version, "spec", "package", "tag"); err != nil {
			return engine.Operation{}, err
		}
		if len(in.Parameters) > 0 {
			err = doc.set(in.Parameters, "spec", "parameters")
		} else {
			doc.unset("spec", "parameters")
		}
		if err != nil {
			return engine.Operation{}, err
		}
		content, err := doc.bytes()
		if err != nil {
			return engine.Operation{}, err
		}
		res, err := a.commit(ctx, gitstore.WriteRequest{
			Changes: []gitstore.Change{{Path: f.path, Content: content, ExpectedRevision: current}},
			Message: fmt.Sprintf("Update %s in project %s", in.Ref.Name, in.Ref.Project),
		})
		if err != nil {
			if opts.ExpectedRevision == "" && errors.Is(err, gitstore.ErrConflict) && attempt < maxCASAttempts {
				continue
			}
			return failure(err, engine.CodeConflict)
		}
		updated, err := parseRelease(f.path, content, in.Ref.Project)
		if err != nil {
			return engine.Operation{}, err
		}
		view := a.pendingView(updated, res.Files[f.path])
		view.Status.Phase = engine.PhaseUpdating
		return engine.Operation{Outcome: engine.OutcomeAccepted, DesiredRevision: res.Files[f.path], Instance: &view}, nil
	}
}

func conflict(ref engine.InstanceRef, expected string) engine.Operation {
	return engine.Rejected(engine.CodeConflict, fmt.Sprintf("instance %q changed since revision %s", ref.Name, expected))
}

// DeleteInstance removes the file. The Release goes when the sync tool
// applies the commit.
func (a *Adapter) DeleteInstance(ctx context.Context, ref engine.InstanceRef, opts engine.WriteOptions) (engine.Operation, error) {
	if opts.ExpectedRevision != "" && !validRevision(opts.ExpectedRevision) {
		return conflict(ref, opts.ExpectedRevision), nil
	}
	f, current, err := a.readRelease(ctx, ref)
	if err != nil {
		if errors.Is(err, engine.ErrNotFound) && a.releaseOnlyInCluster(ctx, ref) {
			return engine.Rejected(engine.CodeConflict, fmt.Sprintf("instance %q exists in the cluster but not in git", ref.Name)), nil
		}
		return engine.Operation{}, err
	}
	if opts.ExpectedRevision != "" && opts.ExpectedRevision != current {
		return conflict(ref, opts.ExpectedRevision), nil
	}
	res, err := a.commit(ctx, gitstore.WriteRequest{
		Changes: []gitstore.Change{{Path: f.path, Delete: true, ExpectedRevision: opts.ExpectedRevision}},
		Message: fmt.Sprintf("Delete %s from project %s", ref.Name, ref.Project),
	})
	if err != nil {
		return failure(err, engine.CodeConflict)
	}
	return engine.Operation{
		Outcome:         engine.OutcomeAccepted,
		DesiredRevision: res.Revision,
		Instance:        &engine.InstanceView{Ref: ref, EngineName: f.release.Name, TargetNamespace: ref.Project, Status: engine.InstanceStatus{Deleting: true}},
	}, nil
}

// releaseOnlyInCluster reports whether ref exists in the cluster as a project
// or sync-managed Release even though it has no file in git.
func (a *Adapter) releaseOnlyInCluster(ctx context.Context, ref engine.InstanceRef) bool {
	releases, err := a.listReleases(ctx, ref.Project)
	if err != nil {
		return false
	}
	for i := range releases {
		r := &releases[i]
		if r.Labels[crd.LabelProject] != ref.Project && !a.appliedFromGit(r) {
			continue
		}
		if id, _ := identity(r); id == ref {
			return true
		}
	}
	return false
}

// readRelease reads an instance from the head of the branch, for a write.
func (a *Adapter) readRelease(ctx context.Context, ref engine.InstanceRef) (*releaseFile, string, error) {
	snap, err := a.snapshots.latest(ctx)
	if err != nil {
		return nil, "", err
	}
	return a.releaseIn(snap, ref)
}

func (a *Adapter) releaseIn(snap *gitstore.Snapshot, ref engine.InstanceRef) (*releaseFile, string, error) {
	files, err := releaseFiles(snap, ref.Project)
	if err != nil {
		return nil, "", err
	}
	f := findRelease(files, ref)
	if f == nil {
		return nil, "", notFound("instance %q not found in project %q", ref.Name, ref.Project)
	}
	rev, err := a.snapshots.fileRevision(snap, f.path)
	if err != nil {
		return nil, "", err
	}
	return f, rev, nil
}

func (a *Adapter) GetInstance(ctx context.Context, ref engine.InstanceRef) (engine.Instance, string, error) {
	snap, err := a.snapshots.refresh(ctx)
	if err != nil {
		return engine.Instance{}, "", err
	}
	f, rev, err := a.releaseIn(snap, ref)
	if err != nil {
		return engine.Instance{}, "", err
	}
	return f.instance(), rev, nil
}

func (a *Adapter) ListInstances(ctx context.Context, project string) ([]engine.Instance, error) {
	snap, err := a.snapshots.current(ctx)
	if err != nil {
		return nil, err
	}
	files, err := releaseFiles(snap, project)
	if err != nil {
		return nil, err
	}
	out := make([]engine.Instance, 0, len(files))
	for _, f := range sortedFiles(files) {
		out = append(out, f.instance())
	}
	return out, nil
}
