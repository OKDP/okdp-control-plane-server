package models

// ExternalSecretRequest is the request body for creating or updating an external secret
type ExternalSecretRequest struct {
	Name            string                    `json:"name"`
	SecretStoreRef  string                    `json:"secretStoreRef" binding:"required"`
	Target          ExternalSecretTarget      `json:"target" binding:"required"`
	RefreshInterval string                    `json:"refreshInterval" binding:"required"`
	Data            []ExternalSecretDataEntry `json:"data" binding:"required"`
}

// ExternalSecretTarget configures the target Kubernetes Secret
type ExternalSecretTarget struct {
	Name string `json:"name" binding:"required"`
}

// ExternalSecretDataEntry defines a single key mapping from remote to local
type ExternalSecretDataEntry struct {
	SecretKey string               `json:"secretKey" binding:"required"`
	RemoteRef ExternalSecretRemote `json:"remoteRef" binding:"required"`
}

// ExternalSecretRemote points to a key in the remote store
type ExternalSecretRemote struct {
	Key      string `json:"key" binding:"required"`
	Property string `json:"property,omitempty"`
}

// ExternalSecretResponse is the API response model for an external secret
type ExternalSecretResponse struct {
	Name            string                       `json:"name"`
	Namespace       string                       `json:"namespace"`
	SecretStoreRef  string                       `json:"secretStoreRef"`
	Target          ExternalSecretTargetResponse `json:"target"`
	RefreshInterval string                       `json:"refreshInterval"`
	Data            []ExternalSecretDataEntry    `json:"data"`
	Status          string                       `json:"status"`
	LastSyncedAt    *string                      `json:"lastSyncedAt"`
	LastError       *string                      `json:"lastError"`
	CreatedAt       string                       `json:"createdAt"`
}

// ExternalSecretTargetResponse includes the creation policy in the response
type ExternalSecretTargetResponse struct {
	Name           string `json:"name"`
	CreationPolicy string `json:"creationPolicy"`
}

// ExternalSecretStatusResponse is the detailed status of an external secret
type ExternalSecretStatusResponse struct {
	Status       string                 `json:"status"`
	Conditions   []SecretStoreCondition `json:"conditions"`
	LastSyncedAt *string                `json:"lastSyncedAt"`
	LastError    *string                `json:"lastError"`
}
