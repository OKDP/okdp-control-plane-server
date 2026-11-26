package kubocdgit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

var connectionGR = crd.GetConnectionGVR().GroupResource()

// ConnectionRepository stores the Connections of a project as
// projects/<project>/connections/<name>.yaml. A read merges git with the
// cluster: a committed Connection carries the status the cluster reports,
// and the Connections only the cluster holds (the ones a Release publishes)
// are listed as they are. ClusterConnections (empty namespace) and the
// credentials Secrets stay in the cluster, through Cluster.
type ConnectionRepository struct {
	adapter *Adapter
	cluster repository.ConnectionRepository
	client  kubernetes.Interface
}

var _ repository.ConnectionRepository = (*ConnectionRepository)(nil)

// NewConnectionRepository returns the git-backed connection repository.
// cluster is the Kubernetes one, client reads and marks the Secrets.
func NewConnectionRepository(adapter *Adapter, cluster repository.ConnectionRepository, client kubernetes.Interface) *ConnectionRepository {
	return &ConnectionRepository{adapter: adapter, cluster: cluster, client: client}
}

func (r *ConnectionRepository) Available(ctx context.Context) bool {
	return r.cluster.Available(ctx)
}

// gitConnections reads the Connections of a project. A file that is not a
// Connection of the project is skipped with a warning or, when strict, is an
// error: a manifest that cannot be read may reference any Secret.
func (r *ConnectionRepository) gitConnections(snap *gitstore.Snapshot, namespace string, strict bool) (map[string]crd.Connection, error) {
	dir, err := gitstore.ConnectionsDir(namespace)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	entries, err := snap.ReadDir(dir)
	if errors.Is(err, gitstore.ErrNotFound) {
		return map[string]crd.Connection{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]crd.Connection{}
	for _, e := range entries {
		manifest := strings.HasSuffix(e.Name, ".yaml") || strings.HasSuffix(e.Name, ".yml")
		name, ok := gitstore.ObjectName(e.Name)
		if e.Kind != gitstore.KindFile || !ok {
			if strict && manifest {
				return nil, fmt.Errorf("%s/%s is not a Connection file", dir, e.Name)
			}
			continue
		}
		c, _, err := readConnection(snap, namespace, name)
		if err != nil {
			if strict {
				return nil, fmt.Errorf("%s/%s: %w", dir, e.Name, err)
			}
			logrus.WithError(err).WithField("connection", namespace+"/"+name).Warn("Skipping a file that is not a Connection of its project")
			continue
		}
		if c == nil {
			continue
		}
		if c.ResourceVersion, err = r.adapter.snapshots.fileRevision(snap, dir+"/"+e.Name); err != nil {
			return nil, err
		}
		out[c.Name] = *c
	}
	return out, nil
}

func readConnection(snap *gitstore.Snapshot, namespace, name string) (*crd.Connection, []byte, error) {
	p, err := gitstore.ConnectionFile(namespace, name)
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
		return nil, nil, err
	}
	var c crd.Connection
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &c); err != nil {
		return nil, nil, err
	}
	if c.Kind != crd.ConnectionKind || c.Name != name || (c.Namespace != "" && c.Namespace != namespace) {
		return nil, nil, fmt.Errorf("%s is not the Connection %s/%s", p, namespace, name)
	}
	c.Namespace = namespace
	return &c, data, nil
}

// clusterConnections lists the cluster side, empty when the CRDs are absent
// or the list fails: git stays readable.
func (r *ConnectionRepository) clusterConnections(ctx context.Context, namespace string) map[string]crd.Connection {
	out := map[string]crd.Connection{}
	if !r.cluster.Available(ctx) {
		return out
	}
	list, err := r.cluster.List(ctx, namespace)
	if err != nil {
		logrus.WithError(err).WithField("namespace", namespace).Debug("Cannot list the Connections of the cluster")
		return out
	}
	for _, c := range list {
		out[c.Name] = c
	}
	return out
}

func (r *ConnectionRepository) List(ctx context.Context, namespace string) ([]crd.Connection, error) {
	if namespace == "" {
		return r.cluster.List(ctx, namespace)
	}
	snap, err := r.adapter.snapshots.current(ctx)
	if err != nil {
		return nil, err
	}
	committed, err := r.gitConnections(snap, namespace, false)
	if err != nil {
		return nil, err
	}
	merged := r.clusterConnections(ctx, namespace)
	for name, c := range committed {
		if observed, ok := merged[name]; ok {
			c.Status = observed.Status
			c.CreationTimestamp = observed.CreationTimestamp
		}
		merged[name] = c
	}
	out := make([]crd.Connection, 0, len(merged))
	for _, c := range merged {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get reads the Connection from the head of the branch, or from the cached
// snapshot when the remote does not answer within the read timeout. Its
// resourceVersion is the revision of the file, which Update then requires.
func (r *ConnectionRepository) Get(ctx context.Context, namespace, name string) (*crd.Connection, error) {
	if namespace == "" {
		return r.cluster.Get(ctx, namespace, name)
	}
	snap, err := r.adapter.snapshots.refresh(ctx)
	if err != nil {
		return nil, err
	}
	c, _, err := readConnection(snap, namespace, name)
	if errors.Is(err, gitstore.ErrInvalidPath) {
		return nil, apierrors.NewNotFound(connectionGR, name)
	}
	if err != nil {
		return nil, err
	}
	if c == nil {
		if !r.cluster.Available(ctx) {
			return nil, apierrors.NewNotFound(connectionGR, name)
		}
		return r.cluster.Get(ctx, namespace, name)
	}
	p, _ := gitstore.ConnectionFile(namespace, name)
	if c.ResourceVersion, err = r.adapter.snapshots.fileRevision(snap, p); err != nil {
		return nil, err
	}
	if r.cluster.Available(ctx) {
		if observed, err := r.cluster.Get(ctx, namespace, name); err == nil {
			c.Status = observed.Status
			c.CreationTimestamp = observed.CreationTimestamp
		}
	}
	return c, nil
}

// CheckUpdate fails with the error Update would return when the Connection is
// not in git: NotFound, or Conflict for a Connection only the cluster holds.
func (r *ConnectionRepository) CheckUpdate(ctx context.Context, namespace, name string) error {
	if namespace == "" {
		return nil
	}
	snap, err := r.adapter.snapshots.refresh(ctx)
	if err != nil {
		return err
	}
	c, _, err := readConnection(snap, namespace, name)
	if errors.Is(err, gitstore.ErrInvalidPath) {
		return apierrors.NewNotFound(connectionGR, name)
	}
	if err != nil {
		return err
	}
	if c == nil {
		return r.notInGit(ctx, namespace, name)
	}
	return nil
}

func connectionManifest(namespace string, c *crd.Connection) (map[string]any, error) {
	spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&c.Spec)
	if err != nil {
		return nil, err
	}
	metadata := map[string]any{"name": c.Name, "namespace": namespace}
	if len(c.Labels) > 0 {
		metadata["labels"] = c.Labels
	}
	if len(c.Annotations) > 0 {
		metadata["annotations"] = c.Annotations
	}
	return map[string]any{
		"apiVersion": crd.ConnectionAPIVersion,
		"kind":       crd.ConnectionKind,
		"metadata":   metadata,
		"spec":       spec,
	}, nil
}

// Create commits the Connection. An unknown outcome is reported as an
// error matching engine.ErrOutcomeUnknown: the Connection may land.
func (r *ConnectionRepository) Create(ctx context.Context, namespace string, c *crd.Connection) error {
	if namespace == "" {
		return r.cluster.Create(ctx, namespace, c)
	}
	p, err := gitstore.ConnectionFile(namespace, c.Name)
	if err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if r.cluster.Available(ctx) {
		if _, err := r.cluster.Get(ctx, namespace, c.Name); err == nil {
			return apierrors.NewAlreadyExists(connectionGR, c.Name)
		}
	}
	manifest, err := connectionManifest(namespace, c)
	if err != nil {
		return err
	}
	content, err := render(manifest)
	if err != nil {
		return err
	}
	_, err = r.adapter.commit(ctx, gitstore.WriteRequest{
		Changes: []gitstore.Change{{Path: p, Content: content, MustNotExist: true}},
		Message: fmt.Sprintf("Create connection %s in project %s", c.Name, namespace),
	})
	return connectionError(err, c.Name)
}

func connectionError(err error, name string) error {
	return gitObjectError(err, connectionGR, name)
}

// gitObjectError is storeError, with an unknown outcome reported as an error
// matching engine.ErrOutcomeUnknown: the object may land.
func gitObjectError(err error, gr schema.GroupResource, name string) error {
	if errors.Is(err, gitstore.ErrUncertain) {
		return &engine.UnknownError{Operation: engine.Operation{Outcome: engine.OutcomeUnknown, Message: err.Error()}}
	}
	return storeError(err, gr, name)
}

// Update sets the labels and annotations c carries and replaces the
// contract, description, values and priority of the committed Connection,
// keeping its other fields. When c carries the file revision Get gave it as
// resourceVersion, a file changed since is a Conflict: c was built from an
// older state and would bring it back.
func (r *ConnectionRepository) Update(ctx context.Context, namespace string, c *crd.Connection) error {
	if namespace == "" {
		return r.cluster.Update(ctx, namespace, c)
	}
	p, err := gitstore.ConnectionFile(namespace, c.Name)
	if err != nil {
		return apierrors.NewNotFound(connectionGR, c.Name)
	}
	for attempt := 0; ; attempt++ {
		snap, err := r.adapter.snapshots.latest(ctx)
		if err != nil {
			return err
		}
		_, data, err := readConnection(snap, namespace, c.Name)
		if err != nil {
			return err
		}
		if data == nil {
			return r.notInGit(ctx, namespace, c.Name)
		}
		rev, err := r.adapter.snapshots.fileRevision(snap, p)
		if err != nil {
			return err
		}
		expected := c.ResourceVersion
		if !validRevision(expected) {
			expected = ""
		}
		if expected != "" && expected != rev {
			return apierrors.NewConflict(connectionGR, c.Name, fmt.Errorf("changed since revision %s", expected))
		}
		content, err := updatedConnection(data, c)
		if err != nil {
			return err
		}
		_, err = r.adapter.commit(ctx, gitstore.WriteRequest{
			Changes: []gitstore.Change{{Path: p, Content: content, ExpectedRevision: rev}},
			Message: fmt.Sprintf("Update connection %s in project %s", c.Name, namespace),
		})
		if errors.Is(err, gitstore.ErrConflict) && expected == "" && attempt < maxCASAttempts {
			continue
		}
		return connectionError(err, c.Name)
	}
}

func updatedConnection(data []byte, c *crd.Connection) ([]byte, error) {
	doc, err := parseDocument(data)
	if err != nil {
		return nil, err
	}
	setOrUnset := func(empty bool, value any, path ...string) error {
		if empty {
			doc.unset(path...)
			return nil
		}
		return doc.set(value, path...)
	}
	var steps []error
	// The labels and annotations c carries are set one by one, the other
	// keys of the file stay.
	for _, k := range sortedKeys(c.Labels) {
		steps = append(steps, doc.set(c.Labels[k], "metadata", "labels", k))
	}
	for _, k := range sortedKeys(c.Annotations) {
		steps = append(steps, doc.set(c.Annotations[k], "metadata", "annotations", k))
	}
	steps = append(steps,
		doc.set(c.Spec.Contract, "spec", "contract"),
		setOrUnset(c.Spec.Description == "", c.Spec.Description, "spec", "description"),
		setOrUnset(len(c.Spec.Values) == 0, c.Spec.Values, "spec", "values"),
		setOrUnset(c.Spec.Priority == 0, c.Spec.Priority, "spec", "priority"),
	)
	if err := errors.Join(steps...); err != nil {
		return nil, err
	}
	return doc.bytes()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// notInGit refuses a change to a Connection the cluster holds and git does not.
func (r *ConnectionRepository) notInGit(ctx context.Context, namespace, name string) error {
	if r.cluster.Available(ctx) {
		if _, err := r.cluster.Get(ctx, namespace, name); err == nil {
			return apierrors.NewConflict(connectionGR, name, errors.New("the connection exists in the cluster but not in git"))
		}
	}
	return apierrors.NewNotFound(connectionGR, name)
}

func (r *ConnectionRepository) Delete(ctx context.Context, namespace, name string) error {
	if namespace == "" {
		return r.cluster.Delete(ctx, namespace, name)
	}
	p, err := gitstore.ConnectionFile(namespace, name)
	if err != nil {
		return apierrors.NewNotFound(connectionGR, name)
	}
	_, err = r.adapter.commit(ctx, gitstore.WriteRequest{
		Changes: []gitstore.Change{{Path: p, Delete: true}},
		Message: fmt.Sprintf("Delete connection %s from project %s", name, namespace),
	})
	if errors.Is(err, gitstore.ErrNotFound) {
		return r.notInGit(ctx, namespace, name)
	}
	return connectionError(err, name)
}

// --- credentials Secrets, kept in the cluster ---

func (r *ConnectionRepository) CreateOrUpdateSecret(ctx context.Context, namespace, name string, data map[string][]byte) error {
	err := r.cluster.CreateOrUpdateSecret(ctx, namespace, name, data)
	if apierrors.IsNotFound(err) {
		return namespaceProvisioningError(namespace)
	}
	return err
}

func (r *ConnectionRepository) GetSecretData(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	if namespace == "" {
		return nil, fmt.Errorf("a namespace is required to read credentials")
	}
	secret, err := r.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if secret.Labels[crd.LabelManagedBy] != crd.ManagedByValue {
		return nil, fmt.Errorf("%w: secret %q in namespace %q", repository.ErrForeignSecret, name, namespace)
	}
	out := make(map[string][]byte, len(secret.Data))
	for k, v := range secret.Data {
		out[k] = append([]byte(nil), v...)
	}
	return out, nil
}

func (r *ConnectionRepository) DeleteSecret(ctx context.Context, namespace, name string) error {
	return r.cluster.DeleteSecret(ctx, namespace, name)
}

func (r *ConnectionRepository) InspectSecret(ctx context.Context, namespace, name string) (repository.SecretContent, bool, error) {
	return r.cluster.InspectSecret(ctx, namespace, name)
}

// ReleaseCredentials marks a credentials Secret the control plane owns
// instead of deleting it, see releaseCredentials.
func (r *ConnectionRepository) ReleaseCredentials(ctx context.Context, namespace, name string) error {
	return releaseCredentials(ctx, r.client, namespace, name)
}

// credentialsReferences returns the Secrets of namespace the Connections of
// git and of the cluster reference. A cluster that does not serve Connections
// holds none; any other failed list is an error, never an empty answer.
func (r *ConnectionRepository) credentialsReferences(ctx context.Context, snap *gitstore.Snapshot, namespace string) (map[string]bool, error) {
	refs := map[string]bool{}
	add := func(c crd.Connection) {
		if ref := c.Annotations[crd.AnnotationCredentialsSecret]; ref != "" {
			if ns, name, ok := strings.Cut(ref, "/"); ok && ns == namespace {
				refs[name] = true
			}
		}
		if name, ok := c.Spec.Values["secretRef"].(string); ok && name != "" {
			refs[name] = true
		}
	}
	committed, err := r.gitConnections(snap, namespace, true)
	if err != nil {
		return nil, err
	}
	for _, c := range committed {
		add(c)
	}
	list, err := r.adapter.cluster.Resource(crd.GetConnectionGVR()).Namespace(namespace).List(ctx, metav1.ListOptions{})
	switch {
	case err == nil:
		for i := range list.Items {
			var c crd.Connection
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &c); err != nil {
				return nil, fmt.Errorf("the Connections of the cluster cannot be decoded: %w", err)
			}
			add(c)
		}
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
	default:
		return nil, fmt.Errorf("the Connections of the cluster cannot be listed: %w", err)
	}
	return refs, nil
}
