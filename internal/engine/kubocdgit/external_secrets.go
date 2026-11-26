package kubocdgit

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

var externalSecretGR = crd.GetExternalSecretGVR().GroupResource()

// ExternalSecretRepository stores the ExternalSecrets of a project as
// projects/<project>/external-secrets/<name>.yaml, merged on read with the
// status the cluster reports. An ExternalSecret only the cluster holds is
// listed as it is, but never changed from here.
type ExternalSecretRepository struct {
	adapter *Adapter
	cluster repository.ExternalSecretRepository
	objects projectObjects
}

var _ repository.ExternalSecretRepository = (*ExternalSecretRepository)(nil)

// NewExternalSecretRepository returns the git-backed ExternalSecret
// repository. cluster is the Kubernetes one.
func NewExternalSecretRepository(adapter *Adapter, cluster repository.ExternalSecretRepository) *ExternalSecretRepository {
	return &ExternalSecretRepository{
		adapter: adapter,
		cluster: cluster,
		objects: projectObjects{
			adapter: adapter,
			kind:    crd.ExternalSecretKind,
			dir:     gitstore.ExternalSecretsDir,
			file:    gitstore.ExternalSecretFile,
		},
	}
}

func (r *ExternalSecretRepository) Available(ctx context.Context) bool {
	return r.cluster.Available(ctx)
}

func decodeExternalSecret(obj *committedObject, observed *crd.ESOExternalSecret) (*crd.ESOExternalSecret, error) {
	var es crd.ESOExternalSecret
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.object, &es); err != nil {
		return nil, err
	}
	es.ResourceVersion = obj.revision
	if observed != nil {
		es.Status = observed.Status
		es.CreationTimestamp = observed.CreationTimestamp
	}
	return &es, nil
}

func (r *ExternalSecretRepository) clusterObjects(ctx context.Context, namespace string) map[string]crd.ESOExternalSecret {
	out := map[string]crd.ESOExternalSecret{}
	if !r.cluster.Available(ctx) {
		return out
	}
	list, err := r.cluster.List(ctx, namespace)
	if err != nil {
		logrus.WithError(err).WithField("namespace", namespace).Debug("Cannot list the ExternalSecrets of the cluster")
		return out
	}
	for _, es := range list {
		out[es.Name] = es
	}
	return out
}

func (r *ExternalSecretRepository) List(ctx context.Context, namespace string) ([]crd.ESOExternalSecret, error) {
	snap, err := r.adapter.snapshots.current(ctx)
	if err != nil {
		return nil, err
	}
	committed, err := r.objects.list(snap, namespace, false)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	merged := r.clusterObjects(ctx, namespace)
	for name, obj := range committed {
		var observed *crd.ESOExternalSecret
		if es, ok := merged[name]; ok {
			observed = &es
		}
		es, err := decodeExternalSecret(obj, observed)
		if err != nil {
			logrus.WithError(err).WithField("file", obj.path).Warn("Skipping an ExternalSecret that could not be decoded")
			continue
		}
		merged[name] = *es
	}
	out := make([]crd.ESOExternalSecret, 0, len(merged))
	for _, es := range merged {
		out = append(out, es)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get reads the ExternalSecret from the head of the branch, or from the
// cached snapshot when the remote does not answer within the read timeout.
func (r *ExternalSecretRepository) Get(ctx context.Context, namespace, name string) (*crd.ESOExternalSecret, error) {
	snap, err := r.adapter.snapshots.refresh(ctx)
	if err != nil {
		return nil, err
	}
	obj, err := r.objects.read(snap, namespace, name)
	if errors.Is(err, gitstore.ErrInvalidPath) {
		return nil, apierrors.NewNotFound(externalSecretGR, name)
	}
	if err != nil {
		return nil, err
	}
	if obj == nil {
		if !r.cluster.Available(ctx) {
			return nil, apierrors.NewNotFound(externalSecretGR, name)
		}
		return r.cluster.Get(ctx, namespace, name)
	}
	var observed *crd.ESOExternalSecret
	if r.cluster.Available(ctx) {
		if es, err := r.cluster.Get(ctx, namespace, name); err == nil {
			observed = es
		}
	}
	return decodeExternalSecret(obj, observed)
}

// Create commits the ExternalSecret. An unknown outcome is reported as an
// error matching engine.ErrOutcomeUnknown.
func (r *ExternalSecretRepository) Create(ctx context.Context, namespace string, es *crd.ESOExternalSecret) error {
	p, err := gitstore.ExternalSecretFile(namespace, es.Name)
	if err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if r.cluster.Available(ctx) {
		if _, err := r.cluster.Get(ctx, namespace, es.Name); err == nil {
			return apierrors.NewAlreadyExists(externalSecretGR, es.Name)
		}
	}
	spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&es.Spec)
	if err != nil {
		return err
	}
	content, err := render(newManifest(crd.ExternalSecretAPIVersion, crd.ExternalSecretKind, namespace, es.Name, es.Labels, es.Annotations, spec))
	if err != nil {
		return err
	}
	_, err = r.adapter.commit(ctx, gitstore.WriteRequest{
		Changes: []gitstore.Change{{Path: p, Content: content, MustNotExist: true}},
		Message: fmt.Sprintf("Create external secret %s in project %s", es.Name, namespace),
	})
	return gitObjectError(err, externalSecretGR, es.Name)
}

// Update replaces what the console edits in the committed ExternalSecret
// (refresh interval, store reference, target name and creation policy, the
// data entries, the labels and annotations es carries) and keeps its other
// fields. A data entry keeps the fields of the committed entry with the same
// secretKey. When es carries the file revision Get gave it as
// resourceVersion, a file changed since is a Conflict.
func (r *ExternalSecretRepository) Update(ctx context.Context, namespace string, es *crd.ESOExternalSecret) error {
	p, err := gitstore.ExternalSecretFile(namespace, es.Name)
	if err != nil {
		return apierrors.NewNotFound(externalSecretGR, es.Name)
	}
	expected := es.ResourceVersion
	if !validRevision(expected) {
		expected = ""
	}
	for attempt := 0; ; attempt++ {
		snap, err := r.adapter.snapshots.latest(ctx)
		if err != nil {
			return err
		}
		obj, err := r.objects.read(snap, namespace, es.Name)
		if err != nil {
			return err
		}
		if obj == nil {
			return r.notInGit(ctx, namespace, es.Name)
		}
		if expected != "" && expected != obj.revision {
			return apierrors.NewConflict(externalSecretGR, es.Name, fmt.Errorf("changed since revision %s", expected))
		}
		content, err := updatedExternalSecret(obj, es)
		if err != nil {
			return err
		}
		_, err = r.adapter.commit(ctx, gitstore.WriteRequest{
			Changes: []gitstore.Change{{Path: p, Content: content, ExpectedRevision: obj.revision}},
			Message: fmt.Sprintf("Update external secret %s in project %s", es.Name, namespace),
		})
		if errors.Is(err, gitstore.ErrConflict) && expected == "" && attempt < maxCASAttempts {
			continue
		}
		return gitObjectError(err, externalSecretGR, es.Name)
	}
}

func updatedExternalSecret(obj *committedObject, es *crd.ESOExternalSecret) ([]byte, error) {
	doc, err := parseDocument(obj.data)
	if err != nil {
		return nil, err
	}
	if err := setMetadata(doc, es.Labels, es.Annotations); err != nil {
		return nil, err
	}
	spec := es.Spec
	steps := []error{
		doc.set(spec.SecretStoreRef.Name, "spec", "secretStoreRef", "name"),
		doc.set(spec.SecretStoreRef.Kind, "spec", "secretStoreRef", "kind"),
		doc.set(spec.Target.Name, "spec", "target", "name"),
		doc.set(spec.Target.CreationPolicy, "spec", "target", "creationPolicy"),
		doc.set(mergedData(obj.object, spec.Data), "spec", "data"),
	}
	if spec.RefreshInterval != "" {
		steps = append(steps, doc.set(spec.RefreshInterval, "spec", "refreshInterval"))
	} else {
		doc.unset("spec", "refreshInterval")
	}
	if err := errors.Join(steps...); err != nil {
		return nil, err
	}
	cleanManifest(doc)
	return doc.bytes()
}

// mergedData is the data list of the request, each entry built on the
// committed entry with the same secretKey so that the fields the console
// does not edit stay.
func mergedData(committed map[string]any, data []crd.ESOExternalSecretData) []any {
	spec, _ := committed["spec"].(map[string]any)
	existing := map[string]map[string]any{}
	if list, ok := spec["data"].([]any); ok {
		for _, raw := range list {
			if entry, ok := raw.(map[string]any); ok {
				if key, ok := entry["secretKey"].(string); ok {
					existing[key] = entry
				}
			}
		}
	}
	out := make([]any, 0, len(data))
	for _, d := range data {
		entry := map[string]any{}
		for k, v := range existing[d.SecretKey] {
			entry[k] = v
		}
		remote := map[string]any{}
		if old, ok := entry["remoteRef"].(map[string]any); ok {
			for k, v := range old {
				remote[k] = v
			}
		}
		remote["key"] = d.RemoteRef.Key
		if d.RemoteRef.Property != "" {
			remote["property"] = d.RemoteRef.Property
		} else {
			delete(remote, "property")
		}
		entry["secretKey"] = d.SecretKey
		entry["remoteRef"] = remote
		out = append(out, entry)
	}
	return out
}

// notInGit refuses a change to an ExternalSecret the cluster holds and git
// does not.
func (r *ExternalSecretRepository) notInGit(ctx context.Context, namespace, name string) error {
	if r.cluster.Available(ctx) {
		if _, err := r.cluster.Get(ctx, namespace, name); err == nil {
			return apierrors.NewConflict(externalSecretGR, name, errors.New("the external secret exists in the cluster but not in git"))
		}
	}
	return apierrors.NewNotFound(externalSecretGR, name)
}

func (r *ExternalSecretRepository) Delete(ctx context.Context, namespace, name string) error {
	p, err := gitstore.ExternalSecretFile(namespace, name)
	if err != nil {
		return apierrors.NewNotFound(externalSecretGR, name)
	}
	_, err = r.adapter.commit(ctx, gitstore.WriteRequest{
		Changes: []gitstore.Change{{Path: p, Delete: true}},
		Message: fmt.Sprintf("Delete external secret %s from project %s", name, namespace),
	})
	if errors.Is(err, gitstore.ErrNotFound) {
		return r.notInGit(ctx, namespace, name)
	}
	return gitObjectError(err, externalSecretGR, name)
}
