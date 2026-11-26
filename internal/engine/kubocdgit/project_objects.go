package kubocdgit

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
)

// projectObjects reads the manifests of one kind a project holds in a
// directory of its own, one file per object.
type projectObjects struct {
	adapter *Adapter
	kind    string
	dir     func(project string) (string, error)
	file    func(project, name string) (string, error)
}

// committedObject is a manifest of git with the revision of its file.
type committedObject struct {
	object   map[string]any
	data     []byte
	path     string
	revision string
}

// read returns the object name of project, nil when git does not hold it.
// A file that is not that object is an error.
func (o projectObjects) read(snap *gitstore.Snapshot, project, name string) (*committedObject, error) {
	p, err := o.file(project, name)
	if err != nil {
		return nil, err
	}
	data, err := snap.ReadFile(p)
	if errors.Is(err, gitstore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	obj, err := decode(data)
	if err != nil {
		return nil, err
	}
	kind, _ := obj["kind"].(string)
	meta, _ := obj["metadata"].(map[string]any)
	objName, _ := meta["name"].(string)
	objNamespace, _ := meta["namespace"].(string)
	if kind != o.kind || objName != name || (objNamespace != "" && objNamespace != project) {
		return nil, fmt.Errorf("%s is not the %s %s/%s", p, o.kind, project, name)
	}
	if meta != nil {
		meta["namespace"] = project
	}
	rev, err := o.adapter.snapshots.fileRevision(snap, p)
	if err != nil {
		return nil, err
	}
	return &committedObject{object: obj, data: data, path: p, revision: rev}, nil
}

// list reads every object of project. A file that is not one of them is
// skipped with a warning or, when strict, is an error: a decision resting on
// the whole directory cannot ignore a manifest it failed to read.
func (o projectObjects) list(snap *gitstore.Snapshot, project string, strict bool) (map[string]*committedObject, error) {
	dir, err := o.dir(project)
	if err != nil {
		return nil, err
	}
	entries, err := snap.ReadDir(dir)
	if errors.Is(err, gitstore.ErrNotFound) {
		return map[string]*committedObject{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]*committedObject{}
	for _, e := range entries {
		manifest := strings.HasSuffix(e.Name, ".yaml") || strings.HasSuffix(e.Name, ".yml")
		name, ok := gitstore.ObjectName(e.Name)
		if e.Kind != gitstore.KindFile || !ok {
			if strict && manifest {
				return nil, fmt.Errorf("%s/%s is not a %s file", dir, e.Name, o.kind)
			}
			continue
		}
		obj, err := o.read(snap, project, name)
		if err != nil {
			if strict {
				return nil, fmt.Errorf("%s/%s: %w", dir, e.Name, err)
			}
			logrus.WithError(err).WithField("file", dir+"/"+e.Name).Warnf("Skipping a file that is not a %s of its project", o.kind)
			continue
		}
		if obj != nil {
			out[name] = obj
		}
	}
	return out, nil
}

// clusterKey reports whether a label or annotation key is one the sync tools
// or kubectl write on a cluster object.
func clusterKey(key string) bool {
	return strings.HasPrefix(key, "kustomize.toolkit.fluxcd.io/") ||
		strings.HasPrefix(key, "argocd.argoproj.io/") ||
		key == "kubectl.kubernetes.io/last-applied-configuration"
}

// serverFields are the metadata fields the API server and the controllers
// set on a cluster object.
var serverFields = []string{
	"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields",
	"deletionTimestamp", "deletionGracePeriodSeconds", "ownerReferences", "finalizers",
}

// cleanManifest drops from a manifest about to be committed what the API
// server, the controllers and the sync tools add to a cluster object.
func cleanManifest(doc *document) {
	doc.unset("status")
	for _, field := range serverFields {
		doc.unset("metadata", field)
	}
	for _, field := range []string{"labels", "annotations"} {
		doc.unsetKeys(clusterKey, "metadata", field)
	}
}

// setMetadata sets on doc the labels and annotations given, one key at a
// time: the other keys of the file stay.
func setMetadata(doc *document, labels, annotations map[string]string) error {
	var steps []error
	for _, k := range sortedKeys(labels) {
		steps = append(steps, doc.set(labels[k], "metadata", "labels", k))
	}
	for _, k := range sortedKeys(annotations) {
		steps = append(steps, doc.set(annotations[k], "metadata", "annotations", k))
	}
	return errors.Join(steps...)
}

// newManifest is a manifest of kind with only the metadata the control
// plane owns.
func newManifest(apiVersion, kind, namespace, name string, labels, annotations map[string]string, spec map[string]any) map[string]any {
	metadata := map[string]any{"name": name, "namespace": namespace}
	if len(labels) > 0 {
		metadata["labels"] = labels
	}
	if len(annotations) > 0 {
		metadata["annotations"] = annotations
	}
	return map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   metadata,
		"spec":       spec,
	}
}

// collectSecretRefs adds to refs the name of every secret reference found in
// v: a mapping under a key ending in secretRef (tokenSecretRef,
// secretRef...) that carries a name.
func collectSecretRefs(v any, refs map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if strings.HasSuffix(strings.ToLower(k), "secretref") {
				if m, ok := child.(map[string]any); ok {
					if name, ok := m["name"].(string); ok && name != "" {
						refs[name] = true
					}
				}
			}
			collectSecretRefs(child, refs)
		}
	case []any:
		for _, child := range t {
			collectSecretRefs(child, refs)
		}
	}
}
