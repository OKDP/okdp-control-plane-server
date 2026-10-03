package crd

// Labels the server sets on the objects it manages.
const (
	// LabelProject carries the project an object belongs to.
	LabelProject = "okdp.io/project"

	// LabelManagedBy marks the objects this server owns (credentials Secrets).
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedByValue is the value of LabelManagedBy for objects we create.
	ManagedByValue = "okdp-control-plane"
)
