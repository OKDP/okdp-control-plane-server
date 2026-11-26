package models

// --- Platform Services (core OKDP, full lifecycle management) ---

// PlatformService is a managed service available in the OKDP data platform,
// read from the Context under serviceCatalog.categories[].services. Category
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

// MenuCategory describes a console navigation section, read from the Context
// (spec.context.serviceCatalog.categories). Key and Label both carry the
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
type ServiceUpdateRequest struct {
	Tag        string         `json:"tag,omitempty"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

// ServiceInstance represents a deployed platform service (KuboCD Release) for a project.
// StatusMessage carries a human-readable explanation when Status is not "Ready"
// (typically the latest K8s Warning event in the target namespace, e.g. a Helm
// upgrade failure or an invalid parameter rejected by the API server).
type ServiceInstance struct {
	Name            string         `json:"name"`
	ReleaseName     string         `json:"releaseName"`
	Service         string         `json:"service"`
	ServiceTag      string         `json:"serviceTag"`
	Status          string         `json:"status"`
	StatusMessage   string         `json:"statusMessage,omitempty"`
	TargetNamespace string         `json:"targetNamespace"`
	URL             string         `json:"url,omitempty"`
	Roles           []string       `json:"roles,omitempty"`
	Parameters      map[string]any `json:"parameters,omitempty"`
	// Connections is what the release actually resolved, published by the
	// controller. Without it the console can only show what a service asked
	// for, never what it runs against.
	Connections []ServiceConnection `json:"connections,omitempty"`
	CreatedAt   string              `json:"createdAt,omitempty"`
	// Phase is the state in the vocabulary every deployment engine shares:
	// Pending (accepted, not acted on yet), Installing, Updating, Ready,
	// Degraded (ready but its pods fail), Error, Suspended, Deleting, or
	// Unknown (could not be observed). Status keeps its own four values.
	Phase string `json:"phase"`
	// The three revisions are opaque and empty when the engine cannot tell.
	// DesiredRevision is the stored desired state (KuboCD: the Release
	// generation, git: the commit), PublishedRevision what a build step made of
	// it, AppliedRevision what the engine proved it applied.
	DesiredRevision   string `json:"desiredRevision"`
	PublishedRevision string `json:"publishedRevision,omitempty"`
	AppliedRevision   string `json:"appliedRevision"`
	// Operation is set on the answer to a write only.
	Operation *Operation `json:"operation,omitempty"`
}

// ServiceConnection is one connection a deployed service is bound to.
type ServiceConnection struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Kind separates a Connection from a ClusterConnection, which may share a
	// name, and tells the console which page to link to.
	Kind string `json:"kind"`
	// Resolved is false while the release is still waiting for it.
	Resolved bool `json:"resolved"`
	// State is how far the connection got: declared (exists, declared by a
	// user), published (exists, published by the service providing it), bound
	// (the service is bound to it) or ready (bound, and its provider is
	// ready). Absent when the connection does not exist or the engine cannot
	// tell. Of several candidates of one name, the furthest one counts.
	State string `json:"state,omitempty"`
}

// ServiceVersions lists the versions of a catalog service and the one
// deployed by default.
type ServiceVersions struct {
	Versions []string `json:"versions"`
	Default  string   `json:"default"`
}
