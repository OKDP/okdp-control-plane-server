package models

// User represents an identity user for API responses/requests
type User struct {
	Name     string   `json:"name"`     // Display Name / Full Name
	Username string   `json:"username"` // ID / Login
	Email    []string `json:"email,omitempty"`
	Comment  string   `json:"comment,omitempty"`
	Disabled bool     `json:"disabled,omitempty"`
	UID      int      `json:"uid,omitempty"`
	// Groups is a computed field (memberships), useful for API
	Groups []string `json:"groups,omitempty"`
	// Password is write-only, used for creation/update; credentials are
	// managed by the identity backend and never stored by this server.
	Password string `json:"password,omitempty"`
}

// Group represents an identity group for API responses/requests
type Group struct {
	Name        string `json:"name"`
	Comment     string `json:"comment,omitempty"`
	Description string `json:"description,omitempty"` // Alias for Comment if needed
}

// GroupBinding represents the link between a user and a group
type GroupBinding struct {
	User  string `json:"user"`
	Group string `json:"group"`
}
