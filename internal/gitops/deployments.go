package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

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
	instanceYAML, err := EncodeInstance(st.Instance)
	if err != nil {
		return err
	}
	valuesYAML, err := EncodeValues(st.Values)
	if err != nil {
		return err
	}
	generated, err := d.Flux.RenderInstance(st.Instance)
	if err != nil {
		return err
	}
	files := map[string][]byte{InstanceFile: instanceYAML, ValuesFile: valuesYAML}
	for name, data := range generated {
		files[name] = data
	}
	for name, data := range files {
		if err := tx.WriteFile(path.Join(dir, name), data); err != nil {
			return err
		}
	}
	return nil
}

// CreateInstance declares a new instance. Fails with ErrExists when the
// instance directory is already there.
func (d *Deployments) CreateInstance(ctx context.Context, user string, st InstanceState) (string, error) {
	target := st.Instance.Project + "/" + st.Instance.Name
	return d.Store.Update(ctx, CommitMessage("deploy", target, user), func(tx Tx) error {
		if tx.Exists(ServiceDir(st.Instance.Project, st.Instance.Name)) {
			return fmt.Errorf("instance %s: %w", target, ErrExists)
		}
		return d.writeInstance(tx, &st)
	})
}

// UpdateInstance applies change to the latest declaration of an instance and
// writes it back. change may run more than once when the write is replayed.
func (d *Deployments) UpdateInstance(ctx context.Context, user, project, name string, change func(st *InstanceState) error) (*InstanceState, string, error) {
	var result *InstanceState
	rev, err := d.Store.Update(ctx, CommitMessage("update", project+"/"+name, user), func(tx Tx) error {
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
func (d *Deployments) DeleteInstance(ctx context.Context, user, project, name string) (string, error) {
	target := project + "/" + name
	return d.Store.Update(ctx, CommitMessage("delete", target, user), func(tx Tx) error {
		dir := ServiceDir(project, name)
		if !tx.Exists(dir) {
			return fmt.Errorf("instance %s: %w", target, ErrNotFound)
		}
		return tx.Remove(dir)
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
		if e == KustomizationFile || !strings.HasSuffix(e, ".yaml") {
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

func (d *Deployments) renderConnections(tx Tx, project string) error {
	names, err := connectionNames(tx, project)
	if err != nil {
		return err
	}
	dir := ConnectionsDir(project)
	generated, err := d.Flux.RenderConnections(project, names)
	if err != nil {
		return err
	}
	if _, ok := generated[KustomizationFile]; !ok {
		if err := tx.Remove(path.Join(dir, KustomizationFile)); err != nil {
			return err
		}
	}
	for name, data := range generated {
		if err := tx.WriteFile(path.Join(dir, name), data); err != nil {
			return err
		}
	}
	return nil
}

// PutConnection writes an external connection. create fails with ErrExists
// when it is already declared; an update fails with ErrNotFound when it is not.
func (d *Deployments) PutConnection(ctx context.Context, user string, c Connection, create bool) (string, error) {
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
	return d.Store.Update(ctx, CommitMessage(action, target, user), func(tx Tx) error {
		exists := tx.Exists(ConnectionPath(c.Project, c.Name))
		if create && exists {
			return fmt.Errorf("connection %s: %w", target, ErrExists)
		}
		if !create && !exists {
			return fmt.Errorf("connection %s: %w", target, ErrNotFound)
		}
		data, err := EncodeConnection(c)
		if err != nil {
			return err
		}
		if err := tx.WriteFile(ConnectionPath(c.Project, c.Name), data); err != nil {
			return err
		}
		return d.renderConnections(tx, c.Project)
	})
}

// DeleteConnection removes an external connection. It refuses while an
// instance still layers it in: both engines would fail to render that instance.
func (d *Deployments) DeleteConnection(ctx context.Context, user, project, name string) (string, error) {
	target := project + "/" + name
	return d.Store.Update(ctx, CommitMessage("delete connection", target, user), func(tx Tx) error {
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
		return d.renderConnections(tx, project)
	})
}

// --- Projects ---

// Project is projects/<p>/project.yaml.
type Project struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
}

// PutProject writes project.yaml, creating the project directory if needed.
func (d *Deployments) PutProject(ctx context.Context, user string, p Project) (string, error) {
	if err := ValidateName("project", p.Name); err != nil {
		return "", err
	}
	data, err := MarshalYAML(p)
	if err != nil {
		return "", err
	}
	return d.Store.Update(ctx, CommitMessage("update project", p.Name, user), func(tx Tx) error {
		return tx.WriteFile(ProjectFilePath(p.Name), data)
	})
}

// DeleteProject removes a project and everything it declares.
func (d *Deployments) DeleteProject(ctx context.Context, user, project string) (string, error) {
	if err := ValidateName("project", project); err != nil {
		return "", err
	}
	return d.Store.Update(ctx, CommitMessage("delete project", project, user), func(tx Tx) error {
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
func (d *Deployments) UpdateCatalog(ctx context.Context, user, target string, change func(catalog map[string]any) error) (string, error) {
	return d.Store.Update(ctx, CommitMessage("update catalog", target, user), func(tx Tx) error {
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
		data, err := MarshalYAML(catalog)
		if err != nil {
			return err
		}
		if bytes.Equal(data, raw) {
			return nil
		}
		return tx.WriteFile(CatalogPath, data)
	})
}
