package models

// --- Platform Services (core OKDP, full lifecycle management) ---

// PlatformService is a managed service available in the OKDP data platform (from Context okdp.services).
type PlatformService struct {
	Name           string   `json:"name"`
	Versions       []string `json:"versions"`
	DefaultVersion string   `json:"defaultVersion"`
	Description    string   `json:"description"`
	Icon           string   `json:"icon,omitempty"`
	Category       string   `json:"category,omitempty"`
	Repository     string   `json:"repository,omitempty"`
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
	CreatedAt       string         `json:"createdAt,omitempty"`
}

// --- Catalog (client self-service, no OKDP management) ---

// CatalogCategory groups packages that clients can deploy on their own.
type CatalogCategory struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Packages    []CatalogPackage `json:"packages"`
}

// CatalogPackage is a deployable package in the self-service catalog.
type CatalogPackage struct {
	Name string `json:"name"`
	Tag  string `json:"tag"`
}
