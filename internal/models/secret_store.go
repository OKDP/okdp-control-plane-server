package models

// SecretStoreRequest is the request body for creating or updating a secret store
type SecretStoreRequest struct {
	Name      string           `json:"name"`
	Provider  string           `json:"provider" binding:"required"`
	Vault     *VaultConfig     `json:"vault" binding:"required"`
	Auth      *SecretStoreAuth `json:"auth" binding:"required"`
	IsDefault bool             `json:"isDefault,omitempty"`
}

// VaultConfig holds Vault-specific provider configuration
type VaultConfig struct {
	Server   string `json:"server" binding:"required"`
	Path     string `json:"path" binding:"required"`
	Version  string `json:"version" binding:"required"`
	CABundle string `json:"caBundle,omitempty"`
}

// SecretStoreAuth holds authentication configuration
type SecretStoreAuth struct {
	Type   string           `json:"type" binding:"required"`
	Config SecretAuthConfig `json:"config" binding:"required"`
}

// SecretAuthConfig holds auth-method-specific parameters
type SecretAuthConfig struct {
	Token     string `json:"token,omitempty"`
	MountPath string `json:"mountPath,omitempty"`
	Role      string `json:"role,omitempty"`
}

// SecretStoreResponse is the API response model for a secret store
type SecretStoreResponse struct {
	Name          string                   `json:"name"`
	Provider      string                   `json:"provider"`
	Namespace     string                   `json:"namespace"`
	Status        string                   `json:"status"`
	LastCheckedAt *string                  `json:"lastCheckedAt"`
	LastError     *string                  `json:"lastError"`
	IsDefault     bool                     `json:"isDefault"`
	CreatedAt     string                   `json:"createdAt"`
	Vault         *VaultConfig             `json:"vault,omitempty"`
	Auth          *SecretStoreAuthResponse `json:"auth,omitempty"`
}

// SecretStoreAuthResponse is the auth config returned in GET responses (token is NEVER included)
type SecretStoreAuthResponse struct {
	Type   string               `json:"type"`
	Config SecretAuthConfigSafe `json:"config"`
}

// SecretAuthConfigSafe is the sanitized auth config that never exposes secrets
type SecretAuthConfigSafe struct {
	MountPath *string `json:"mountPath"`
	Role      *string `json:"role"`
}

// SecretStoreStatusResponse is the detailed status of a secret store
type SecretStoreStatusResponse struct {
	Status        string                 `json:"status"`
	Conditions    []SecretStoreCondition `json:"conditions"`
	LastCheckedAt *string                `json:"lastCheckedAt"`
	LastError     *string                `json:"lastError"`
}

// SecretStoreCondition represents a single ESO condition entry
type SecretStoreCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

// TestConnectionResponse is the response for the test-connection endpoint
type TestConnectionResponse struct {
	Message string `json:"message"`
}
