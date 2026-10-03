package gitops

import (
	"bytes"
	"fmt"
	"path"
	"regexp"

	"gopkg.in/yaml.v3"
	sigsyaml "sigs.k8s.io/yaml"
)

// Paths of the deployments repository layout (shared no-kubocd contract),
// relative to GITOPS_PATH.
const (
	PlatformValuesPath = "platform/platform-values.yaml"
	CatalogPath        = "platform/catalog.yaml"
	ProjectsDir        = "projects"

	InstanceFile      = "instance.yaml"
	ValuesFile        = "values.yaml"
	HelmReleaseFile   = "helmrelease.yaml"
	KustomizationFile = "kustomization.yaml"
	ProjectFile       = "project.yaml"
)

// ComponentsDir holds the platform components (written by administrators only).
const ComponentsDir = "platform/components"

// ReleasesNamespace is where Flux HelmReleases and their value ConfigMaps live.
const ReleasesNamespace = "okdp-releases"

// PlatformValuesConfigMap is the ConfigMap carrying platform/platform-values.yaml
// under Flux, key ValuesKey.
const PlatformValuesConfigMap = "okdp-platform-values"

// ValuesKey is the key of every values ConfigMap.
const ValuesKey = "values.yaml"

// Validation rules of instance.yaml, the same as render-flux.sh enforces.
var (
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	chartRef     = regexp.MustCompile(`^oci://[a-z0-9]([-a-z0-9.]*[a-z0-9])?(:[0-9]+)?(/[a-z0-9]([-a-z0-9._]*[a-z0-9])?)+$`)
	chartVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][-0-9A-Za-z.]*)?$`)
)

// ValidateName refuses a name that cannot be a path segment, a Kubernetes
// object name and a Helm release name part at once.
func ValidateName(kind, name string) error {
	if len(name) == 0 || len(name) > 63 || !dnsLabel.MatchString(name) {
		return fmt.Errorf("invalid %s name %q: it must be a lowercase DNS label", kind, name)
	}
	return nil
}

func ProjectDir(project string) string { return path.Join(ProjectsDir, project) }
func ProjectKustomizationPath(project string) string {
	return path.Join(ProjectDir(project), KustomizationFile)
}
func ProjectFilePath(project string) string {
	return path.Join(ProjectDir(project), ProjectFile)
}
func ServicesDir(project string) string { return path.Join(ProjectDir(project), "services") }
func ServiceDir(project, instance string) string {
	return path.Join(ServicesDir(project), instance)
}
func ConnectionsDir(project string) string { return path.Join(ProjectDir(project), "connections") }
func ConnectionPath(project, name string) string {
	return path.Join(ConnectionsDir(project), name+".yaml")
}

// ReleaseName is the Helm release name of a project service.
func ReleaseName(project, instance string) string { return project + "-" + instance }

// Instance is the engine-neutral instance.yaml, the source both engines render from.
type Instance struct {
	// Name is the instance name; the Helm release is <project>-<name>.
	Name string `yaml:"name" json:"name"`
	// Project is the project, which is also the target namespace.
	Project string `yaml:"project" json:"project"`
	// Service is the chart name.
	Service string `yaml:"service" json:"service"`
	// Chart is the OCI chart reference without tag (oci://registry/path/<chart>).
	Chart string `yaml:"chart" json:"chart"`
	// Version is the chart version.
	Version string `yaml:"version" json:"version"`
	// Connections are the external connection files layered in, in order.
	Connections []string `yaml:"connections" json:"connections"`
}

// ReleaseName is the Helm release name of the instance.
func (i Instance) ReleaseName() string { return ReleaseName(i.Project, i.Name) }

// Validate checks the rules of instance.yaml (okdp-sandbox/gitops/README.md),
// the ones render-flux.sh enforces: a file it would reject must never be
// committed.
func (i Instance) Validate() error {
	if err := ValidateName("project", i.Project); err != nil {
		return err
	}
	if err := ValidateName("instance", i.Name); err != nil {
		return err
	}
	if err := ValidateName("service", i.Service); err != nil {
		return err
	}
	if len(i.ReleaseName()) > 53 {
		return fmt.Errorf("the release name %q is longer than the 53 characters Helm allows", i.ReleaseName())
	}
	if !chartRef.MatchString(i.Chart) {
		return fmt.Errorf("chart %q is not an oci:// chart reference", i.Chart)
	}
	if path.Base(i.Chart) != i.Service {
		return fmt.Errorf("chart %q must end with /%s", i.Chart, i.Service)
	}
	if !chartVersion.MatchString(i.Version) {
		return fmt.Errorf("version %q is not an exact semantic version", i.Version)
	}
	seen := map[string]bool{}
	for _, c := range i.Connections {
		if err := ValidateName("connection", c); err != nil {
			return err
		}
		if seen[c] {
			return fmt.Errorf("connection %q listed twice", c)
		}
		seen[c] = true
	}
	return nil
}

// MarshalYAML renders a document the way every file of the repository is
// rendered: yaml.v3, two-space indentation (the yq v4 defaults).
func MarshalYAML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// EncodeInstance renders instance.yaml.
func EncodeInstance(i Instance) ([]byte, error) {
	if i.Connections == nil {
		i.Connections = []string{}
	}
	return MarshalYAML(i)
}

// DecodeInstance parses instance.yaml.
func DecodeInstance(data []byte) (*Instance, error) {
	var i Instance
	if err := yaml.Unmarshal(data, &i); err != nil {
		return nil, fmt.Errorf("invalid instance.yaml: %w", err)
	}
	return &i, nil
}

// EncodeValues renders a values file. An empty set of values is "{}".
func EncodeValues(values map[string]any) ([]byte, error) {
	if values == nil {
		values = map[string]any{}
	}
	return MarshalYAML(values)
}

// DecodeValues parses a values file into JSON-compatible types (numbers are
// float64, maps are map[string]any), the shape the JSON schema validator and
// the REST API expect.
func DecodeValues(data []byte) (map[string]any, error) {
	values := map[string]any{}
	if len(bytes.TrimSpace(data)) == 0 {
		return values, nil
	}
	if err := sigsyaml.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("invalid values file: %w", err)
	}
	if values == nil {
		values = map[string]any{}
	}
	return values, nil
}
