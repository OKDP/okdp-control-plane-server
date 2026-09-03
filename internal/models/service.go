package models

// --- Platform Services (core OKDP, full lifecycle management) ---

// PlatformService is a managed service available in the OKDP data platform,
// read from platform/catalog.yaml under categories[].services. Category
// is the title of the section the service sits in.
type PlatformService struct {
	Name           string   `json:"name"`
	Versions       []string `json:"versions"`
	DefaultVersion string   `json:"defaultVersion"`
	Description    string   `json:"description"`
	Icon           string   `json:"icon,omitempty"`
	Category       string   `json:"category,omitempty"`
	Repository     string   `json:"repository,omitempty"`
	// Label is the display name shown in the console menu; the service `name`
	// (its identity and route) is used when empty.
	Label string `json:"label,omitempty"`
	// ExposesUI is false for infrastructure-only packages (operators, storage
	// backends) that have no console page, and nil when the catalog does not set
	// it, which the console treats as exposing a UI. It lets the catalog-driven
	// menu leave purely infrastructural packages out of the navigation.
	ExposesUI *bool `json:"exposesUI,omitempty"`
}

// MenuCategory describes a console navigation section, read from
// platform/catalog.yaml (categories). Key and Label both carry the
// section title, Order its position in the list, so the console renders an
// ordered, labeled menu driven by the catalog.
type MenuCategory struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Icon  string `json:"icon,omitempty"`
	Order int    `json:"order"`
}

// ProfileImage represents an available container image for a JupyterHub profile type.
type ProfileImage struct {
	Label string `json:"label"`
	Image string `json:"image"`
}

// ServiceRequest is the body for POST /api/projects/:name/services (deploy a platform service).
type ServiceRequest struct {
	Service      string         `json:"service" binding:"required"`
	Tag          string         `json:"tag,omitempty"`
	InstanceName string         `json:"instanceName,omitempty"`
	Parameters   map[string]any `json:"parameters,omitempty"`
}

// ServiceUpdateRequest is the body for PATCH /api/projects/:name/services/:serviceName/parameters.
// Parameters is a JSON Merge Patch (RFC 7386) of the stored values: null
// deletes a key, objects merge recursively, arrays replace.
type ServiceUpdateRequest struct {
	Tag        string         `json:"tag,omitempty"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

// ServiceInstance is a service instance of a project: its declaration in the
// deployments Git repository, and what the cluster reports about it.
//
// Status is one of Pending (committed to Git, not yet picked up by the GitOps
// engine), Installing, Updating, Ready or Error. StatusMessage explains a
// status other than Ready (the engine's condition message, or the latest
// Kubernetes Warning event of the instance).
type ServiceInstance struct {
	Name            string `json:"name"`
	ReleaseName     string `json:"releaseName"`
	Service         string `json:"service"`
	ServiceTag      string `json:"serviceTag"`
	Status          string `json:"status"`
	StatusMessage   string `json:"statusMessage,omitempty"`
	TargetNamespace string `json:"targetNamespace"`
	URL             string `json:"url,omitempty"`
	// Roles is no longer filled: KuboCD package roles have no successor.
	Roles      []string       `json:"roles,omitempty"`
	Parameters map[string]any `json:"parameters,omitempty"`
	// Usage is the rendered Markdown the chart publishes in its instance
	// descriptor (the former package usage).
	Usage string `json:"usage,omitempty"`
	// Connections lists what the instance is wired to: the external
	// connections its declaration layers in, and the connections of other
	// instances its parameters name.
	Connections []ServiceConnection `json:"connections,omitempty"`
	CreatedAt   string              `json:"createdAt,omitempty"`
	// Revision is the Git commit holding the change, set on the responses of
	// a deployment or an update only.
	Revision string `json:"revision,omitempty"`
}

// Kinds of ServiceConnection.
const (
	// ConnectionKindExternal is a connection declared in the project
	// (projects/<p>/connections/<name>.yaml).
	ConnectionKindExternal = "Connection"
	// ConnectionKindInternal is a connection provided by another instance of
	// the project (its descriptor's outputs).
	ConnectionKindInternal = "Instance"
)

// ServiceConnection is one connection a deployed service is bound to.
type ServiceConnection struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Kind is Connection (external, declared in the project) or Instance
	// (provided by another instance), and tells the console which page to
	// link to.
	Kind string `json:"kind"`
	// Resolved is false while the connection does not exist.
	Resolved bool `json:"resolved"`
}
