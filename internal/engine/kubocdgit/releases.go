package kubocdgit

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

// releaseFile is a Release manifest read from git.
type releaseFile struct {
	// path is where the file is, relative to the store path. It is not
	// always <name>.yaml for a file pushed by hand.
	path    string
	data    []byte
	release crd.Release
	ref     engine.InstanceRef
	service string
}

// releaseFiles reads the Releases of a project. A file that is not a
// Release of this project is skipped with a warning, it does not hide the
// others. Two files for one Release keep the one named after it.
func releaseFiles(snap *gitstore.Snapshot, project string) (map[string]*releaseFile, error) {
	dir, err := gitstore.ReleasesDir(project)
	if err != nil {
		return nil, err
	}
	entries, err := snap.ReadDir(dir)
	if errors.Is(err, gitstore.ErrNotFound) {
		return map[string]*releaseFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]*releaseFile{}
	for _, e := range entries {
		fileName, ok := gitstore.ObjectName(e.Name)
		if e.Kind != gitstore.KindFile || !ok {
			continue
		}
		p := dir + "/" + e.Name
		log := logrus.WithField("file", p)
		data, err := snap.ReadFile(p)
		if err != nil {
			return nil, err
		}
		f, err := parseRelease(p, data, project)
		if err != nil {
			log.WithError(err).Warn("Skipping a file that is not a Release of its project")
			continue
		}
		if prev, dup := out[f.release.Name]; dup {
			log.WithField("other", prev.path).Warn("Two files hold the same Release, keeping the one named after it")
			if fileName != f.release.Name {
				continue
			}
		}
		out[f.release.Name] = f
	}
	return out, nil
}

func parseRelease(p string, data []byte, project string) (*releaseFile, error) {
	obj, err := decode(data)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var release crd.Release
	if err := json.Unmarshal(raw, &release); err != nil {
		return nil, err
	}
	switch {
	case release.Kind != crd.ReleaseKind:
		return nil, fmt.Errorf("kind %q", release.Kind)
	case release.Name == "":
		return nil, errors.New("no metadata.name")
	case release.Namespace != "" && release.Namespace != project:
		return nil, fmt.Errorf("namespace %q", release.Namespace)
	}
	release.Namespace = project
	ref, service := identity(&release)
	return &releaseFile{path: p, data: data, release: release, ref: ref, service: service}, nil
}

// identity names the instance and the service of a Release. The labels the
// control plane writes win. A Release pushed without them is the instance
// named after the Release, without the project prefix the control plane
// adds, of the service named by the last segment of its package repository.
func identity(r *crd.Release) (engine.InstanceRef, string) {
	service := r.Labels[crd.LabelService]
	if service == "" {
		service = path.Base(strings.TrimRight(r.Spec.Package.Repository, "/"))
		if service == "." || service == "/" {
			service = ""
		}
	}
	name := r.Labels[crd.LabelInstanceName]
	if name == "" {
		name = strings.TrimPrefix(r.Name, r.Namespace+"-")
		if name == "" {
			name = r.Name
		}
	}
	return engine.InstanceRef{Project: r.Namespace, Name: name}, service
}

// findRelease looks an instance up by its name, not by the file name.
func findRelease(files map[string]*releaseFile, ref engine.InstanceRef) *releaseFile {
	if f, ok := files[releaseName(ref)]; ok && f.ref == ref {
		return f
	}
	for _, f := range sortedFiles(files) {
		if f.ref == ref {
			return f
		}
	}
	return nil
}

func sortedFiles(files map[string]*releaseFile) []*releaseFile {
	out := make([]*releaseFile, 0, len(files))
	for _, f := range files {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].release.Name < out[j].release.Name })
	return out
}

func (f *releaseFile) instance() engine.Instance {
	return engine.Instance{
		Ref:        f.ref,
		Service:    f.service,
		Version:    f.release.Spec.Package.Tag,
		Parameters: f.release.Spec.Parameters,
	}
}

func releaseName(ref engine.InstanceRef) string {
	return ref.Project + "-" + ref.Name
}
