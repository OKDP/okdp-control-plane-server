package models

// RenderedValues are the values one okdp.vendor.render of an instance passed
// to a vendored upstream chart, read from its values ConfigMap
// (<release>-<chart>-values, label okdp.io/vendor-values), next to the
// vendored chart's default values they were merged over.
type RenderedValues struct {
	// Name is the values ConfigMap.
	Name string `json:"name" example:"demo-trino-trino-values"`
	// Chart is the vendored chart (its directory under vendor/).
	Chart string `json:"chart" example:"trino"`
	// ChartVersion is the vendored chart, <name>-<version>.
	ChartVersion string `json:"chartVersion" example:"trino-1.42.1"`
	// ServiceVersion is the version of the instance's chart that rendered
	// these values (it can differ from the declared one during an update).
	ServiceVersion string `json:"serviceVersion" example:"480.0.0-1.0.2"`
	// Values is the values.yaml the vendored chart was rendered with.
	Values string `json:"values"`
	// Defaults is the vendored chart's own values.yaml, as published (with
	// its comments). Empty when it could not be read (see DefaultsError).
	Defaults string `json:"defaults,omitempty"`
	// ChangedLines are the 1-based lines of Values that differ from Defaults.
	// Empty when nothing differs or when the defaults could not be read.
	ChangedLines []int `json:"changedLines"`
	// DefaultsError says why the defaults are missing.
	DefaultsError string `json:"defaultsError,omitempty"`
}
