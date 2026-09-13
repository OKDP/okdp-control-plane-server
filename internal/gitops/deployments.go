package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// Deployments reads and writes the deployments repository through its layout.
// It writes exactly the files of the shared contract: nothing else is ever
// created, and generated files are always rewritten from their sources.
type Deployments struct {
	Store Store
	Flux  FluxRenderer
}

// NewDeployments wires a store with the Flux renderer.
func NewDeployments(store Store, flux FluxRenderer) *Deployments {
	if flux == nil {
		flux = NewDefaultFluxRenderer()
	}
	return &Deployments{Store: store, Flux: flux}
}

// InstanceState is what the repository declares for one instance.
type InstanceState struct {
	Instance Instance
	// Values are the user parameters (values.yaml).
	Values map[string]any
}

// ErrReleaseTaken is returned when an instance's release name <p>-<i> is
// already produced by another declaration: project a-b instance c and project
// a instance b-c are both release a-b-c. It is not ErrExists: the instance
// itself is new, its name only collides.
type ErrReleaseTaken struct {
	Release  string
	Project  string
	Instance string
	// Component is set when the colliding declaration is a platform component.
	Component string
}

func (e *ErrReleaseTaken) Error() string {
	if e.Component != "" {
		return fmt.Sprintf("release name '%s' is already used by the platform component '%s'", e.Release, e.Component)
	}
	return fmt.Sprintf("release name '%s' is already used by instance '%s' of project '%s'", e.Release, e.Instance, e.Project)
}

// ErrInUse is returned when deleting something other declarations still use.
type ErrInUse struct {
	What  string
	Users []string
}

func (e *ErrInUse) Error() string {
	return fmt.Sprintf("%s is still used by %s", e.What, strings.Join(e.Users, ", "))
}

// --- Instances ---

func readInstance(r Reader, project, name string) (*InstanceState, error) {
	dir := ServiceDir(project, name)
	raw, err := r.ReadFile(path.Join(dir, InstanceFile))
	if err != nil {
		return nil, err
	}
	inst, err := DecodeInstance(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	// The directory is the identity: a hand-edited file saying otherwise
	// would render a release nobody can find.
	inst.Name, inst.Project = name, project
	values := map[string]any{}
	if rawValues, err := r.ReadFile(path.Join(dir, ValuesFile)); err == nil {
		if values, err = DecodeValues(rawValues); err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return &InstanceState{Instance: *inst, Values: values}, nil
}

func listInstances(r Reader, project string) ([]InstanceState, error) {
	names, err := r.ReadDir(ServicesDir(project))
	if err != nil {
		return nil, err
	}
	var out []InstanceState
	for _, name := range names {
		if !r.Exists(path.Join(ServiceDir(project, name), InstanceFile)) {
			continue
		}
		st, err := readInstance(r, project, name)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, nil
}

// ListInstances returns the instances declared in a project, sorted by name.
func (d *Deployments) ListInstances(ctx context.Context, project string) ([]InstanceState, error) {
	var out []InstanceState
	err := d.Store.View(ctx, func(r Reader) error {
		var err error
		out, err = listInstances(r, project)
		return err
	})
	return out, err
}

// GetInstance returns one instance, or an error wrapping ErrNotFound.
func (d *Deployments) GetInstance(ctx context.Context, project, name string) (*InstanceState, error) {
	var out *InstanceState
	err := d.Store.View(ctx, func(r Reader) error {
		var err error
		out, err = readInstance(r, project, name)
		return err
	})
	return out, err
}

func (d *Deployments) writeInstance(tx Tx, st *InstanceState) error {
	if err := st.Instance.Validate(); err != nil {
		return err
	}
	for _, c := range st.Instance.Connections {
		if !tx.Exists(ConnectionPath(st.Instance.Project, c)) {
			return fmt.Errorf("connection %q is not declared in project %q: %w", c, st.Instance.Project, ErrNotFound)
		}
	}
	dir := ServiceDir(st.Instance.Project, st.Instance.Name)

	// The sources are rewritten only when their content changes, so a
	// hand-written file keeps its comments through edits that leave it alone
	// (a version bump does not touch values.yaml).
	if current, err := readInstance(tx, st.Instance.Project, st.Instance.Name); err == nil {
		if !reflect.DeepEqual(current.Instance, normalized(st.Instance)) {
			if err := writeEncoded(tx, path.Join(dir, InstanceFile), st.Instance); err != nil {
				return err
			}
		}
		if !reflect.DeepEqual(current.Values, jsonRoundTrip(st.Values)) || !tx.Exists(path.Join(dir, ValuesFile)) {
			if err := writeValues(tx, path.Join(dir, ValuesFile), st.Values); err != nil {
				return err
			}
		}
	} else {
		if err := writeEncoded(tx, path.Join(dir, InstanceFile), st.Instance); err != nil {
			return err
		}
		if err := writeValues(tx, path.Join(dir, ValuesFile), st.Values); err != nil {
			return err
		}
	}

	generated, err := d.Flux.RenderInstance(st.Instance)
	if err != nil {
		return err
	}
	for name, data := range generated {
		if err := tx.WriteFile(path.Join(dir, name), data); err != nil {
			return err
		}
	}
	return nil
}

func normalized(i Instance) Instance {
	if i.Connections == nil {
		i.Connections = []string{}
	}
	return i
}

func writeEncoded(tx Tx, p string, i Instance) error {
	data, err := EncodeInstance(i)
	if err != nil {
		return err
	}
	return tx.WriteFile(p, data)
}

func writeValues(tx Tx, p string, values map[string]any) error {
	data, err := EncodeValues(values)
	if err != nil {
		return err
	}
	return tx.WriteFile(p, data)
}

// jsonRoundTrip gives values the types DecodeValues produces, for comparison.
func jsonRoundTrip(values map[string]any) map[string]any {
	data, err := EncodeValues(values)
	if err != nil {
		return values
	}
	out, err := DecodeValues(data)
	if err != nil {
		return values
	}
	return out
}

// CreateInstance declares a new instance. Fails with ErrExists when the
// instance directory is already there.
func (d *Deployments) CreateInstance(ctx context.Context, actor auth.Actor, st InstanceState) (string, error) {
	target := st.Instance.Project + "/" + st.Instance.Name
	return d.Store.Update(ctx, NewCommit("deploy", target, actor), func(tx Tx) error {
		if tx.Exists(ServiceDir(st.Instance.Project, st.Instance.Name)) {
			return fmt.Errorf("instance %s: %w", target, ErrExists)
		}
		if err := checkReleaseFree(tx, st.Instance); err != nil {
			return err
		}
		if err := ensureProject(tx, st.Instance.Project); err != nil {
			return err
		}
		if err := d.writeInstance(tx, &st); err != nil {
			return err
		}
		return d.renderProject(tx, st.Instance.Project)
	})
}

// UpdateInstance applies change to the latest declaration of an instance and
// writes it back. change may run more than once when the write is replayed.
func (d *Deployments) UpdateInstance(ctx context.Context, actor auth.Actor, project, name string, change func(st *InstanceState) error) (*InstanceState, string, error) {
	var result *InstanceState
	rev, err := d.Store.Update(ctx, NewCommit("update", project+"/"+name, actor), func(tx Tx) error {
		st, err := readInstance(tx, project, name)
		if err != nil {
			return err
		}
		if err := change(st); err != nil {
			return err
		}
		st.Instance.Name, st.Instance.Project = name, project
		result = st
		return d.writeInstance(tx, st)
	})
	return result, rev, err
}

// DeleteInstance removes an instance directory. The engine prunes what it
// had deployed.
func (d *Deployments) DeleteInstance(ctx context.Context, actor auth.Actor, project, name string) (string, error) {
	target := project + "/" + name
	return d.Store.Update(ctx, NewCommit("delete", target, actor), func(tx Tx) error {
		dir := ServiceDir(project, name)
		if !tx.Exists(dir) {
			return fmt.Errorf("instance %s: %w", target, ErrNotFound)
		}
		if err := tx.Remove(dir); err != nil {
			return err
		}
		return d.renderProject(tx, project)
	})
}

// --- Connections ---

// Connection is an external connection file (projects/<p>/connections/<name>.yaml).
type Connection struct {
	Name        string
	Project     string
	Contract    string
	Description string
	// Values are the non-secret fields of the contract.
	Values map[string]any
	// SecretRef names the Secret, in the project namespace, holding the secret
	// fields. Empty when the contract has none.
	SecretRef string
}

const descriptionComment = "# description: "

// EncodeConnection renders a connection file:
//
//	# description: <text>          (only when set)
//	connections:
//	  <name>:
//	    contract: <contract>
//	    <fields, sorted>
//	    secretRef:
//	      name: <secret>
func EncodeConnection(c Connection) ([]byte, error) {
	entry := mapping("contract", str(c.Contract))
	keys := make([]string, 0, len(c.Values))
	for k := range c.Values {
		if k == "contract" || k == "secretRef" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var node yaml.Node
		if err := node.Encode(c.Values[k]); err != nil {
			return nil, fmt.Errorf("field %q: %w", k, err)
		}
		entry.Content = append(entry.Content, str(k), &node)
	}
	if c.SecretRef != "" {
		entry.Content = append(entry.Content, str("secretRef"), mapping("name", str(c.SecretRef)))
	}
	doc, err := encodeDocuments(mapping("connections", mapping(c.Name, entry)))
	if err != nil {
		return nil, err
	}
	if c.Description == "" {
		return doc, nil
	}
	description := strings.Join(strings.Fields(c.Description), " ")
	return append([]byte(descriptionComment+description+"\n"), doc...), nil
}

// DecodeConnection parses a connection file named name.
func DecodeConnection(project, name string, data []byte) (*Connection, error) {
	values, err := DecodeValues(data)
	if err != nil {
		return nil, err
	}
	all, _ := values["connections"].(map[string]any)
	entry, ok := all[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("connection file %s.yaml does not declare connections.%s", name, name)
	}
	c := &Connection{Name: name, Project: project, Values: map[string]any{}}
	for k, v := range entry {
		switch k {
		case "contract":
			c.Contract, _ = v.(string)
		case "secretRef":
			if ref, ok := v.(map[string]any); ok {
				c.SecretRef, _ = ref["name"].(string)
			}
		default:
			c.Values[k] = v
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "#") {
			break
		}
		if strings.HasPrefix(line, descriptionComment) {
			c.Description = strings.TrimPrefix(line, descriptionComment)
		}
	}
	return c, nil
}

func connectionNames(r Reader, project string) ([]string, error) {
	entries, err := r.ReadDir(ConnectionsDir(project))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !strings.HasSuffix(e, ".yaml") {
			continue
		}
		names = append(names, strings.TrimSuffix(e, ".yaml"))
	}
	return names, nil
}

// ListConnections returns the external connections of a project, sorted by name.
func (d *Deployments) ListConnections(ctx context.Context, project string) ([]Connection, error) {
	var out []Connection
	err := d.Store.View(ctx, func(r Reader) error {
		names, err := connectionNames(r, project)
		if err != nil {
			return err
		}
		for _, name := range names {
			raw, err := r.ReadFile(ConnectionPath(project, name))
			if err != nil {
				return err
			}
			c, err := DecodeConnection(project, name, raw)
			if err != nil {
				return err
			}
			out = append(out, *c)
		}
		return nil
	})
	return out, err
}

// GetConnection returns one external connection, or an error wrapping ErrNotFound.
func (d *Deployments) GetConnection(ctx context.Context, project, name string) (*Connection, error) {
	var out *Connection
	err := d.Store.View(ctx, func(r Reader) error {
		raw, err := r.ReadFile(ConnectionPath(project, name))
		if err != nil {
			return err
		}
		out, err = DecodeConnection(project, name, raw)
		return err
	})
	return out, err
}

// renderProject rewrites projects/<p>/kustomization.yaml (Flux): the services
// holding an instance.yaml and every connection file, sorted.
func (d *Deployments) renderProject(tx Tx, project string) error {
	if !tx.Exists(ProjectDir(project)) {
		return nil
	}
	names, err := tx.ReadDir(ServicesDir(project))
	if err != nil {
		return err
	}
	var services []string
	for _, name := range names {
		if tx.Exists(path.Join(ServiceDir(project, name), InstanceFile)) {
			services = append(services, name)
		}
	}
	connections, err := connectionNames(tx, project)
	if err != nil {
		return err
	}
	sort.Strings(services)
	sort.Strings(connections)
	data, err := d.Flux.RenderProject(project, services, connections)
	if err != nil {
		return err
	}
	return tx.WriteFile(ProjectKustomizationPath(project), data)
}

// ensureProject declares a project that has no project.yaml yet (one created
// before the deployments repository existed): render-flux.sh refuses a
// project directory without it.
func ensureProject(tx Tx, project string) error {
	if tx.Exists(ProjectFilePath(project)) {
		return nil
	}
	data, err := MarshalYAML(Project{Name: project})
	if err != nil {
		return err
	}
	return tx.WriteFile(ProjectFilePath(project), data)
}

// checkReleaseFree refuses an instance whose release name, or whose values
// ConfigMap in okdp-releases, another declaration already produces:
// project a-b instance c and project a instance b-c are both release a-b-c.
func checkReleaseFree(tx Tx, inst Instance) error {
	release := inst.ReleaseName()
	projects, err := tx.ReadDir(ProjectsDir)
	if err != nil {
		return err
	}
	for _, p := range projects {
		names, err := tx.ReadDir(ServicesDir(p))
		if err != nil {
			return err
		}
		for _, i := range names {
			if (p != inst.Project || i != inst.Name) && ReleaseName(p, i) == release && tx.Exists(path.Join(ServiceDir(p, i), InstanceFile)) {
				return &ErrReleaseTaken{Release: release, Project: p, Instance: i}
			}
		}
	}
	components, err := tx.ReadDir(ComponentsDir)
	if err != nil {
		return err
	}
	for _, c := range components {
		raw, err := tx.ReadFile(path.Join(ComponentsDir, c, InstanceFile))
		if err != nil {
			continue
		}
		other, err := DecodeInstance(raw)
		if err == nil && other.ReleaseName() == release {
			return &ErrReleaseTaken{Release: release, Project: other.Project, Instance: other.Name, Component: c}
		}
	}
	return nil
}

// checkConnectionFree refuses a connection whose ConfigMap conn-<p>-<c>
// another project's connection already produces.
func checkConnectionFree(tx Tx, project, name string) error {
	cm := ConnectionValuesName(project, name)
	projects, err := tx.ReadDir(ProjectsDir)
	if err != nil {
		return err
	}
	for _, p := range projects {
		if p == project {
			continue
		}
		names, err := connectionNames(tx, p)
		if err != nil {
			return err
		}
		for _, c := range names {
			if ConnectionValuesName(p, c) == cm {
				return fmt.Errorf("ConfigMap %s is already produced by projects/%s/connections/%s.yaml: %w", cm, p, c, ErrExists)
			}
		}
	}
	return nil
}

// PutConnection writes an external connection. create fails with ErrExists
// when it is already declared; an update fails with ErrNotFound when it is not.
func (d *Deployments) PutConnection(ctx context.Context, actor auth.Actor, c Connection, create bool) (string, error) {
	if err := ValidateName("project", c.Project); err != nil {
		return "", err
	}
	if err := ValidateName("connection", c.Name); err != nil {
		return "", err
	}
	action := "update connection"
	if create {
		action = "create connection"
	}
	target := c.Project + "/" + c.Name
	return d.Store.Update(ctx, NewCommit(action, target, actor), func(tx Tx) error {
		exists := tx.Exists(ConnectionPath(c.Project, c.Name))
		if create && exists {
			return fmt.Errorf("connection %s: %w", target, ErrExists)
		}
		if !create && !exists {
			return fmt.Errorf("connection %s: %w", target, ErrNotFound)
		}
		if create {
			if err := checkConnectionFree(tx, c.Project, c.Name); err != nil {
				return err
			}
			if err := ensureProject(tx, c.Project); err != nil {
				return err
			}
		}
		data, err := EncodeConnection(c)
		if err != nil {
			return err
		}
		if err := tx.WriteFile(ConnectionPath(c.Project, c.Name), data); err != nil {
			return err
		}
		return d.renderProject(tx, c.Project)
	})
}

// DeleteConnection removes an external connection. It refuses while an
// instance still layers it in: both engines would fail to render that instance.
func (d *Deployments) DeleteConnection(ctx context.Context, actor auth.Actor, project, name string) (string, error) {
	target := project + "/" + name
	return d.Store.Update(ctx, NewCommit("delete connection", target, actor), func(tx Tx) error {
		if !tx.Exists(ConnectionPath(project, name)) {
			return fmt.Errorf("connection %s: %w", target, ErrNotFound)
		}
		instances, err := listInstances(tx, project)
		if err != nil {
			return err
		}
		var users []string
		for _, st := range instances {
			for _, c := range st.Instance.Connections {
				if c == name {
					users = append(users, st.Instance.Name)
				}
			}
		}
		if len(users) > 0 {
			return &ErrInUse{What: "connection " + name, Users: users}
		}
		if err := tx.Remove(ConnectionPath(project, name)); err != nil {
			return err
		}
		return d.renderProject(tx, project)
	})
}

// --- Projects ---

// Project is projects/<p>/project.yaml. A project exists when that file
// does, whoever wrote it: the console and a person editing Git declare
// projects the same way. The directory is the identity, and the namespace
// the project deploys into.
type Project struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
}

// readProject reads projects/<name>/project.yaml. Keys other than
// description are the file author's and are ignored; a file that is not a
// YAML mapping still declares the project, without description.
func readProject(r Reader, name string) (*Project, error) {
	raw, err := r.ReadFile(ProjectFilePath(name))
	if err != nil {
		return nil, err
	}
	p := &Project{Name: name}
	values, err := DecodeValues(raw)
	if err != nil {
		return p, fmt.Errorf("%s: %w", ProjectFilePath(name), err)
	}
	if description, ok := values["description"].(string); ok {
		p.Description = description
	}
	return p, nil
}

// ListProjects returns the projects declared in the repository (the
// directories of projects/ holding a project.yaml), sorted by name. A
// project.yaml that does not parse still lists its project, without
// description, so one bad file does not hide every project.
func (d *Deployments) ListProjects(ctx context.Context) ([]Project, error) {
	out := []Project{}
	err := d.Store.View(ctx, func(r Reader) error {
		names, err := r.ReadDir(ProjectsDir)
		if err != nil {
			return err
		}
		for _, name := range names {
			if ValidateName("project", name) != nil || !r.Exists(ProjectFilePath(name)) {
				continue
			}
			p, err := readProject(r, name)
			if p == nil {
				return err
			}
			if err != nil {
				logrus.WithError(err).Warn("Listing a project whose project.yaml does not parse")
			}
			out = append(out, *p)
		}
		return nil
	})
	return out, err
}

// GetProject returns one project, or an error wrapping ErrNotFound.
func (d *Deployments) GetProject(ctx context.Context, name string) (*Project, error) {
	if err := ValidateName("project", name); err != nil {
		return nil, fmt.Errorf("project %s: %w", name, ErrNotFound)
	}
	var out *Project
	err := d.Store.View(ctx, func(r Reader) error {
		p, err := readProject(r, name)
		if p != nil && err != nil {
			logrus.WithError(err).Warn("Serving a project whose project.yaml does not parse")
			err = nil
		}
		out = p
		return err
	})
	return out, err
}

// CreateProject writes the project.yaml of a new project, or fails with
// ErrExists when the project is already declared. A directory without
// project.yaml (written before the file existed) is adopted.
func (d *Deployments) CreateProject(ctx context.Context, actor auth.Actor, p Project) (string, error) {
	if err := ValidateName("project", p.Name); err != nil {
		return "", err
	}
	data, err := MarshalYAML(p)
	if err != nil {
		return "", err
	}
	return d.Store.Update(ctx, NewCommit("create project", p.Name, actor), func(tx Tx) error {
		if tx.Exists(ProjectFilePath(p.Name)) {
			return fmt.Errorf("project %s: %w", p.Name, ErrExists)
		}
		if err := tx.WriteFile(ProjectFilePath(p.Name), data); err != nil {
			return err
		}
		return d.renderProject(tx, p.Name)
	})
}

// UpdateProject changes the description of a declared project, or fails
// with ErrNotFound. The other keys and the comments of project.yaml are
// kept; an empty description removes the key.
func (d *Deployments) UpdateProject(ctx context.Context, actor auth.Actor, p Project) (string, error) {
	if err := ValidateName("project", p.Name); err != nil {
		return "", fmt.Errorf("project %s: %w", p.Name, ErrNotFound)
	}
	return d.Store.Update(ctx, NewCommit("update project", p.Name, actor), func(tx Tx) error {
		raw, err := tx.ReadFile(ProjectFilePath(p.Name))
		if err != nil {
			return err
		}
		doc, err := DecodeValues(raw)
		if err != nil || doc == nil {
			doc = map[string]any{}
		}
		doc["name"] = p.Name
		if p.Description == "" {
			delete(doc, "description")
		} else {
			doc["description"] = p.Description
		}
		data, err := mergeIntoDocument(raw, doc)
		if err != nil {
			return err
		}
		if bytes.Equal(data, raw) {
			return nil
		}
		return tx.WriteFile(ProjectFilePath(p.Name), data)
	})
}

// DeleteProject removes a project and everything it declares, or fails with
// ErrNotFound when the repository has no such project directory.
func (d *Deployments) DeleteProject(ctx context.Context, actor auth.Actor, project string) (string, error) {
	if err := ValidateName("project", project); err != nil {
		return "", fmt.Errorf("project %s: %w", project, ErrNotFound)
	}
	return d.Store.Update(ctx, NewCommit("delete project", project, actor), func(tx Tx) error {
		if !tx.Exists(ProjectDir(project)) {
			return fmt.Errorf("project %s: %w", project, ErrNotFound)
		}
		return tx.Remove(ProjectDir(project))
	})
}

// --- Platform ---

// ReadPlatformValues returns platform/platform-values.yaml.
func (d *Deployments) ReadPlatformValues(ctx context.Context) ([]byte, error) {
	var out []byte
	err := d.Store.View(ctx, func(r Reader) error {
		var err error
		out, err = r.ReadFile(PlatformValuesPath)
		return err
	})
	return out, err
}

// ReadCatalog returns platform/catalog.yaml as JSON-compatible data.
func (d *Deployments) ReadCatalog(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := d.Store.View(ctx, func(r Reader) error {
		raw, err := r.ReadFile(CatalogPath)
		if err != nil {
			return err
		}
		out, err = DecodeValues(raw)
		return err
	})
	return out, err
}

// UpdateCatalog applies change to the latest catalog and writes it back. A
// missing catalog starts empty.
func (d *Deployments) UpdateCatalog(ctx context.Context, actor auth.Actor, target string, change func(catalog map[string]any) error) (string, error) {
	return d.Store.Update(ctx, NewCommit("update catalog", target, actor), func(tx Tx) error {
		catalog := map[string]any{}
		raw, err := tx.ReadFile(CatalogPath)
		switch {
		case err == nil:
			if catalog, err = DecodeValues(raw); err != nil {
				return err
			}
		case errors.Is(err, ErrNotFound):
		default:
			return err
		}
		if err := change(catalog); err != nil {
			return err
		}
		data, err := mergeIntoDocument(raw, catalog)
		if err != nil {
			return err
		}
		if bytes.Equal(data, raw) {
			return nil
		}
		return tx.WriteFile(CatalogPath, data)
	})
}

// mergeIntoDocument writes updated over the YAML document raw, top-level key
// by top-level key: keys whose value did not change keep their original
// nodes, and the comments of the file (its licence header, the notes a
// person left) survive a console edit. A document that cannot be merged is
// replaced.
func mergeIntoDocument(raw []byte, updated map[string]any) ([]byte, error) {
	var doc yaml.Node
	if len(bytes.TrimSpace(raw)) == 0 || yaml.Unmarshal(raw, &doc) != nil ||
		len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return MarshalYAML(updated)
	}
	root := doc.Content[0]
	original, err := DecodeValues(raw)
	if err != nil {
		return MarshalYAML(updated)
	}

	kept := root.Content[:0]
	seen := map[string]bool{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		newValue, present := updated[key.Value]
		if !present {
			continue
		}
		seen[key.Value] = true
		if !reflect.DeepEqual(jsonRoundTrip(map[string]any{"v": original[key.Value]}), jsonRoundTrip(map[string]any{"v": newValue})) {
			var replacement yaml.Node
			if err := replacement.Encode(newValue); err != nil {
				return nil, err
			}
			replacement.HeadComment, replacement.LineComment, replacement.FootComment = value.HeadComment, value.LineComment, value.FootComment
			value = &replacement
		}
		kept = append(kept, key, value)
	}
	root.Content = kept
	var added []string
	for k := range updated {
		if !seen[k] {
			added = append(added, k)
		}
	}
	sort.Strings(added)
	for _, k := range added {
		var value yaml.Node
		if err := value.Encode(updated[k]); err != nil {
			return nil, err
		}
		root.Content = append(root.Content, str(k), &value)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
