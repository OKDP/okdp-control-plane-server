package kubocdgit

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"

	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

var secretStoreGR = crd.GetSecretStoreGVR().GroupResource()

// SecretStoreRepository stores the SecretStores of a project as
// projects/<project>/secret-stores/<name>.yaml. A read merges git with the
// cluster: a committed store carries the status the cluster reports, and a
// store only the cluster holds is listed as it is, but never changed from
// here. The credentials Secrets stay in the cluster, never in git.
type SecretStoreRepository struct {
	adapter *Adapter
	cluster repository.SecretStoreRepository
	client  kubernetes.Interface
	objects projectObjects
}

var _ repository.SecretStoreRepository = (*SecretStoreRepository)(nil)

// NewSecretStoreRepository returns the git-backed SecretStore repository.
// cluster is the Kubernetes one, client writes and marks the credentials
// Secrets.
func NewSecretStoreRepository(adapter *Adapter, cluster repository.SecretStoreRepository, client kubernetes.Interface) *SecretStoreRepository {
	return &SecretStoreRepository{
		adapter: adapter,
		cluster: cluster,
		client:  client,
		objects: projectObjects{
			adapter: adapter,
			kind:    crd.SecretStoreKind,
			dir:     gitstore.SecretStoresDir,
			file:    gitstore.SecretStoreFile,
		},
	}
}

func (r *SecretStoreRepository) Available(ctx context.Context) bool {
	return r.cluster.Available(ctx)
}

func (r *SecretStoreRepository) decode(obj *committedObject, observed *crd.ESOSecretStore) (*crd.ESOSecretStore, error) {
	store, err := repository.DecodeSecretStore(obj.object)
	if err != nil {
		return nil, err
	}
	store.ResourceVersion = obj.revision
	if observed != nil {
		store.Status = observed.Status
		store.CreationTimestamp = observed.CreationTimestamp
	}
	return store, nil
}

func (r *SecretStoreRepository) clusterStores(ctx context.Context, namespace string) map[string]crd.ESOSecretStore {
	out := map[string]crd.ESOSecretStore{}
	if !r.cluster.Available(ctx) {
		return out
	}
	list, err := r.cluster.List(ctx, namespace)
	if err != nil {
		logrus.WithError(err).WithField("namespace", namespace).Debug("Cannot list the SecretStores of the cluster")
		return out
	}
	for _, s := range list {
		out[s.Name] = s
	}
	return out
}

func (r *SecretStoreRepository) List(ctx context.Context, namespace string) ([]crd.ESOSecretStore, error) {
	snap, err := r.adapter.snapshots.current(ctx)
	if err != nil {
		return nil, err
	}
	committed, err := r.objects.list(snap, namespace, false)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	merged := r.clusterStores(ctx, namespace)
	for name, obj := range committed {
		var observed *crd.ESOSecretStore
		if s, ok := merged[name]; ok {
			observed = &s
		}
		store, err := r.decode(obj, observed)
		if err != nil {
			logrus.WithError(err).WithField("file", obj.path).Warn("Skipping a SecretStore that could not be decoded")
			continue
		}
		merged[name] = *store
	}
	out := make([]crd.ESOSecretStore, 0, len(merged))
	for _, s := range merged {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get reads the store from the head of the branch, or from the cached
// snapshot when the remote does not answer within the read timeout. Its
// resourceVersion is the revision of the file, which Update then requires.
func (r *SecretStoreRepository) Get(ctx context.Context, namespace, name string) (*crd.ESOSecretStore, error) {
	snap, err := r.adapter.snapshots.refresh(ctx)
	if err != nil {
		return nil, err
	}
	obj, err := r.objects.read(snap, namespace, name)
	if errors.Is(err, gitstore.ErrInvalidPath) {
		return nil, apierrors.NewNotFound(secretStoreGR, name)
	}
	if err != nil {
		return nil, err
	}
	if obj == nil {
		if !r.cluster.Available(ctx) {
			return nil, apierrors.NewNotFound(secretStoreGR, name)
		}
		return r.cluster.Get(ctx, namespace, name)
	}
	var observed *crd.ESOSecretStore
	if r.cluster.Available(ctx) {
		if s, err := r.cluster.Get(ctx, namespace, name); err == nil {
			observed = s
		}
	}
	return r.decode(obj, observed)
}

// CheckUpdate fails with the error Update would return when the store is
// not in git: NotFound, or Conflict for a store only the cluster holds.
func (r *SecretStoreRepository) CheckUpdate(ctx context.Context, namespace, name string) error {
	snap, err := r.adapter.snapshots.refresh(ctx)
	if err != nil {
		return err
	}
	obj, err := r.objects.read(snap, namespace, name)
	if errors.Is(err, gitstore.ErrInvalidPath) {
		return apierrors.NewNotFound(secretStoreGR, name)
	}
	if err != nil {
		return err
	}
	if obj == nil {
		return r.notInGit(ctx, namespace, name)
	}
	return nil
}

func secretStoreManifest(namespace string, store *crd.ESOSecretStore) (map[string]any, error) {
	spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&store.Spec)
	if err != nil {
		return nil, err
	}
	return newManifest(crd.SecretStoreAPIVersion, crd.SecretStoreKind, namespace, store.Name, store.Labels, store.Annotations, spec), nil
}

func isDefaultStore(labels map[string]string) bool {
	return labels[crd.LabelDefaultStore] == "true"
}

// clearDefaults returns the changes that drop the default label from every
// store of git but except, each conditioned on the revision read.
func (r *SecretStoreRepository) clearDefaults(snap *gitstore.Snapshot, namespace, except string) ([]gitstore.Change, error) {
	committed, err := r.objects.list(snap, namespace, false)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(committed))
	for name := range committed {
		names = append(names, name)
	}
	sort.Strings(names)
	var changes []gitstore.Change
	for _, name := range names {
		obj := committed[name]
		meta, _ := obj.object["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		if name == except || labels[crd.LabelDefaultStore] != "true" {
			continue
		}
		doc, err := parseDocument(obj.data)
		if err != nil {
			return nil, err
		}
		doc.unsetKeys(func(k string) bool { return k == crd.LabelDefaultStore }, "metadata", "labels")
		content, err := doc.bytes()
		if err != nil {
			return nil, err
		}
		changes = append(changes, gitstore.Change{Path: obj.path, Content: content, ExpectedRevision: obj.revision})
	}
	return changes, nil
}

// Create commits the store. A store labelled default takes the label from
// the other stores of the project in the same commit. An unknown outcome is
// reported as an error matching engine.ErrOutcomeUnknown.
func (r *SecretStoreRepository) Create(ctx context.Context, namespace string, store *crd.ESOSecretStore) error {
	p, err := gitstore.SecretStoreFile(namespace, store.Name)
	if err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if r.cluster.Available(ctx) {
		if _, err := r.cluster.Get(ctx, namespace, store.Name); err == nil {
			return apierrors.NewAlreadyExists(secretStoreGR, store.Name)
		}
	}
	manifest, err := secretStoreManifest(namespace, store)
	if err != nil {
		return err
	}
	content, err := render(manifest)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		changes := []gitstore.Change{{Path: p, Content: content, MustNotExist: true}}
		if isDefaultStore(store.Labels) {
			snap, err := r.adapter.snapshots.latest(ctx)
			if err != nil {
				return err
			}
			cleared, err := r.clearDefaults(snap, namespace, store.Name)
			if err != nil {
				return err
			}
			changes = append(changes, cleared...)
		}
		_, err = r.adapter.commit(ctx, gitstore.WriteRequest{
			Changes: changes,
			Message: fmt.Sprintf("Create secret store %s in project %s", store.Name, namespace),
		})
		if errors.Is(err, gitstore.ErrConflict) && attempt < maxCASAttempts {
			continue
		}
		return gitObjectError(err, secretStoreGR, store.Name)
	}
}

// Update replaces what the console edits in the committed store (the Vault
// server, path, version, CA bundle and authentication, the default label,
// and the labels and annotations store carries) and keeps its other
// fields. When store carries the file revision Get gave it as
// resourceVersion, a file changed since is a Conflict.
func (r *SecretStoreRepository) Update(ctx context.Context, namespace string, store *crd.ESOSecretStore) error {
	p, err := gitstore.SecretStoreFile(namespace, store.Name)
	if err != nil {
		return apierrors.NewNotFound(secretStoreGR, store.Name)
	}
	expected := store.ResourceVersion
	if !validRevision(expected) {
		expected = ""
	}
	for attempt := 0; ; attempt++ {
		snap, err := r.adapter.snapshots.latest(ctx)
		if err != nil {
			return err
		}
		obj, err := r.objects.read(snap, namespace, store.Name)
		if err != nil {
			return err
		}
		if obj == nil {
			return r.notInGit(ctx, namespace, store.Name)
		}
		if expected != "" && expected != obj.revision {
			return apierrors.NewConflict(secretStoreGR, store.Name, fmt.Errorf("changed since revision %s", expected))
		}
		content, err := updatedSecretStore(obj.data, store)
		if err != nil {
			return err
		}
		changes := []gitstore.Change{{Path: p, Content: content, ExpectedRevision: obj.revision}}
		if isDefaultStore(store.Labels) {
			cleared, err := r.clearDefaults(snap, namespace, store.Name)
			if err != nil {
				return err
			}
			changes = append(changes, cleared...)
		}
		_, err = r.adapter.commit(ctx, gitstore.WriteRequest{
			Changes: changes,
			Message: fmt.Sprintf("Update secret store %s in project %s", store.Name, namespace),
		})
		if errors.Is(err, gitstore.ErrConflict) && attempt < maxCASAttempts {
			continue
		}
		return gitObjectError(err, secretStoreGR, store.Name)
	}
}

func updatedSecretStore(data []byte, store *crd.ESOSecretStore) ([]byte, error) {
	doc, err := parseDocument(data)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	for k, v := range store.Labels {
		if k != crd.LabelDefaultStore {
			labels[k] = v
		}
	}
	if err := setMetadata(doc, labels, store.Annotations); err != nil {
		return nil, err
	}
	if isDefaultStore(store.Labels) {
		if err := doc.set("true", "metadata", "labels", crd.LabelDefaultStore); err != nil {
			return nil, err
		}
	} else {
		doc.unsetKeys(func(k string) bool { return k == crd.LabelDefaultStore }, "metadata", "labels")
	}

	vault := store.Spec.Provider.Vault
	if vault == nil {
		return nil, apierrors.NewBadRequest("only the vault provider can be written")
	}
	doc.unsetKeys(func(k string) bool { return k != "vault" }, "spec", "provider")
	base := []string{"spec", "provider", "vault"}
	at := func(path ...string) []string { return append(append([]string{}, base...), path...) }
	steps := []error{
		doc.set(vault.Server, at("server")...),
		doc.set(vault.Path, at("path")...),
		doc.set(vault.Version, at("version")...),
	}
	if len(vault.CABundle) > 0 {
		steps = append(steps, doc.set(base64.StdEncoding.EncodeToString(vault.CABundle), at("caBundle")...))
	} else {
		doc.unset(at("caBundle")...)
	}
	switch {
	case vault.Auth.TokenSecretRef != nil:
		doc.unset(at("auth", "kubernetes")...)
		steps = append(steps,
			doc.set(vault.Auth.TokenSecretRef.Name, at("auth", "tokenSecretRef", "name")...),
			doc.set(vault.Auth.TokenSecretRef.Key, at("auth", "tokenSecretRef", "key")...),
		)
	case vault.Auth.Kubernetes != nil:
		k := vault.Auth.Kubernetes
		doc.unset(at("auth", "tokenSecretRef")...)
		steps = append(steps,
			doc.set(k.MountPath, at("auth", "kubernetes", "mountPath")...),
			doc.set(k.Role, at("auth", "kubernetes", "role")...),
		)
		if k.ServiceAccountRef != nil {
			steps = append(steps, doc.set(k.ServiceAccountRef.Name, at("auth", "kubernetes", "serviceAccountRef", "name")...))
		} else {
			doc.unset(at("auth", "kubernetes", "serviceAccountRef")...)
		}
	}
	if err := errors.Join(steps...); err != nil {
		return nil, err
	}
	cleanManifest(doc)
	return doc.bytes()
}

// notInGit refuses a change to a store the cluster holds and git does not.
func (r *SecretStoreRepository) notInGit(ctx context.Context, namespace, name string) error {
	if r.cluster.Available(ctx) {
		if _, err := r.cluster.Get(ctx, namespace, name); err == nil {
			return apierrors.NewConflict(secretStoreGR, name, errors.New("the secret store exists in the cluster but not in git"))
		}
	}
	return apierrors.NewNotFound(secretStoreGR, name)
}

func (r *SecretStoreRepository) Delete(ctx context.Context, namespace, name string) error {
	p, err := gitstore.SecretStoreFile(namespace, name)
	if err != nil {
		return apierrors.NewNotFound(secretStoreGR, name)
	}
	_, err = r.adapter.commit(ctx, gitstore.WriteRequest{
		Changes: []gitstore.Change{{Path: p, Delete: true}},
		Message: fmt.Sprintf("Delete secret store %s from project %s", name, namespace),
	})
	if errors.Is(err, gitstore.ErrNotFound) {
		return r.notInGit(ctx, namespace, name)
	}
	return gitObjectError(err, secretStoreGR, name)
}

// RemoveDefaultLabel drops the default label from every store of git in one
// commit.
func (r *SecretStoreRepository) RemoveDefaultLabel(ctx context.Context, namespace string) error {
	for attempt := 0; ; attempt++ {
		snap, err := r.adapter.snapshots.latest(ctx)
		if err != nil {
			return err
		}
		changes, err := r.clearDefaults(snap, namespace, "")
		if err != nil {
			return err
		}
		if len(changes) == 0 {
			return nil
		}
		_, err = r.adapter.commit(ctx, gitstore.WriteRequest{
			Changes: changes,
			Message: fmt.Sprintf("Clear the default secret store of project %s", namespace),
		})
		if errors.Is(err, gitstore.ErrConflict) && attempt < maxCASAttempts {
			continue
		}
		return gitObjectError(err, secretStoreGR, namespace)
	}
}

// --- credentials Secrets, kept in the cluster ---

// CreateOrUpdateSecret writes the credentials Secret of a store, labelled as
// owned by the control plane. Writing it again drops the mark a release
// left, so a store created again keeps its credentials. A Secret of that
// name the control plane does not own is refused: taking it over would put
// it under the reaper.
func (r *SecretStoreRepository) CreateOrUpdateSecret(ctx context.Context, namespace, name string, data map[string][]byte) error {
	secrets := r.client.CoreV1().Secrets(namespace)
	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{crd.LabelManagedBy: crd.ManagedByValue},
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		}, metav1.CreateOptions{})
		if apierrors.IsNotFound(err) {
			return namespaceProvisioningError(namespace)
		}
		return err
	}
	if err != nil {
		return err
	}
	if existing.Labels[crd.LabelManagedBy] != crd.ManagedByValue {
		return apierrors.NewConflict(corev1.Resource("secrets"), name,
			errors.New("the credentials secret already exists and is not managed by the control plane"))
	}
	delete(existing.Labels, crd.LabelOrphanedCredentials)
	existing.Data = data
	_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func (r *SecretStoreRepository) GetSecretData(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	return r.cluster.GetSecretData(ctx, namespace, name)
}

func (r *SecretStoreRepository) DeleteSecret(ctx context.Context, namespace, name string) error {
	return r.cluster.DeleteSecret(ctx, namespace, name)
}

// ReleaseCredentials marks a credentials Secret the control plane owns
// instead of deleting it, see releaseCredentials.
func (r *SecretStoreRepository) ReleaseCredentials(ctx context.Context, namespace, name string) error {
	return releaseCredentials(ctx, r.client, namespace, name)
}

// credentialsReferences returns the Secrets the SecretStores of namespace
// reference, in snap (strict) and in the cluster. A cluster that does not
// serve SecretStores holds none, any other failed list is an error.
func (r *SecretStoreRepository) credentialsReferences(ctx context.Context, snap *gitstore.Snapshot, namespace string) (map[string]bool, error) {
	refs := map[string]bool{}
	committed, err := r.objects.list(snap, namespace, true)
	if err != nil {
		return nil, err
	}
	for _, obj := range committed {
		collectSecretRefs(obj.object["spec"], refs)
	}
	list, err := r.adapter.cluster.Resource(crd.GetSecretStoreGVR()).Namespace(namespace).List(ctx, metav1.ListOptions{})
	switch {
	case err == nil:
		for _, item := range list.Items {
			collectSecretRefs(item.Object["spec"], refs)
		}
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
	default:
		return nil, fmt.Errorf("the SecretStores of the cluster cannot be listed: %w", err)
	}
	return refs, nil
}
