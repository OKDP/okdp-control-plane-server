package models

// ConnectionScopeProject is the only scope a connection has: it lives in the
// namespace of the project that declares it. The cluster-wide scope was removed
// on 2026-08-10, nothing consumed it and no ClusterConnection existed.
const ConnectionScopeProject = "project"

// Reasons returned by a connectivity test, so the console can tell a network
// problem from a credential problem instead of showing a raw driver error.
const (
	TestReasonUnreachable   = "unreachable"
	TestReasonAuthFailed    = "auth-failed"
	TestReasonNotFound      = "not-found"
	TestReasonTimeout       = "timeout"
	TestReasonInvalidConfig = "invalid-config"
	TestReasonUnknown       = "unknown"
)

// Field types accepted in a contract descriptor.
const (
	FieldTypeString  = "string"
	FieldTypeNumber  = "number"
	FieldTypeBoolean = "boolean"
	FieldTypeEnum    = "enum"
)

// ConnectionField describes a single input of a contract.
type ConnectionField struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	// Secret says where the value is stored: in the credentials Secret rather
	// than in the Connection spec. It says nothing about how it is typed in.
	Secret bool `json:"secret,omitempty"`
	// Masked hides the value as it is typed. A database user name lives in the
	// Secret because the contract puts it there, but hiding it behind dots only
	// stops the user from checking what they typed.
	Masked      bool     `json:"masked,omitempty"`
	Default     any      `json:"default,omitempty"`
	Options     []string `json:"options,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Help        string   `json:"help,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	// ShowWhen limits the field to one value of another field. A PostgreSQL TLS
	// mode and a MySQL one are not the same list, and offering both at once is
	// how a connection ends up carrying the wrong one.
	ShowWhen *FieldCondition `json:"showWhen,omitempty"`
	// Derived computes the value from another field instead of asking for it.
	// The JDBC driver follows from the engine, and a form that lets the two
	// disagree only produces connections nothing can open.
	Derived *FieldDerivation `json:"derived,omitempty"`
}

// FieldCondition is "that other field equals this value".
type FieldCondition struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// FieldDerivation computes a field from another one through a lookup table.
type FieldDerivation struct {
	From string            `json:"from"`
	Map  map[string]string `json:"map"`
}

// Applies reports whether the field is in play for the given values. A field
// whose condition is not met is neither asked for nor validated.
func (f *ConnectionField) Applies(values map[string]any) bool {
	if f.ShowWhen == nil {
		return true
	}
	current, _ := values[f.ShowWhen.Field].(string)
	return current == f.ShowWhen.Value
}

// ContractDescriptor describes one kind of connection. It drives the form
// rendered by the console, the server-side validation of submitted values, the
// recognition of deployed services as connection providers, and the
// ready-to-use snippets shown when a connection is opened.
type ContractDescriptor struct {
	// Name IS the KuboCD Contract this descriptor produces. One descriptor, one
	// contract, deliberately: an entry form that produced a differently named
	// contract would mean nothing a package asks for could be found by its own
	// name.
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Category    string `json:"category"`
	// External reports whether a user may declare a connection against this
	// contract by hand. A contract that is not external is listed for the
	// internal view, where the connections come from the deployed services,
	// but offers no creation form.
	External bool              `json:"external"`
	Fields   []ConnectionField `json:"fields"`
	// EndpointFrom names the value fields carrying the address a consumer would
	// reach, most specific first. A contract publishing a plain host and port
	// needs none: that convention is the fallback.
	EndpointFrom []string `json:"endpointFrom,omitempty"`
}

// Field returns the named field of the type.
func (t *ContractDescriptor) Field(name string) (*ConnectionField, bool) {
	for i := range t.Fields {
		if t.Fields[i].Name == name {
			return &t.Fields[i], true
		}
	}
	return nil, false
}

// SecretFields lists the fields of the type that hold credentials. Their values
// live in a Kubernetes Secret and never in the Connection spec.
func (t *ContractDescriptor) SecretFields() []string {
	var names []string
	for i := range t.Fields {
		if t.Fields[i].Secret {
			names = append(names, t.Fields[i].Name)
		}
	}
	return names
}

// ConnectionCatalogResponse is the payload of GET /api/contracts.
type ConnectionCatalogResponse struct {
	Types []ContractDescriptor `json:"types"`
	// CRDAvailable reports whether the KuboCD connection CRDs are installed.
	// While they are not, external connections cannot be persisted and the
	// console says so instead of failing on save. Internal connections are
	// derived from deployed services and stay available either way.
	CRDAvailable bool `json:"crdAvailable"`
}

// ConnectionRequest is the body for creating or updating an external connection.
type ConnectionRequest struct {
	Name        string         `json:"name" binding:"required"`
	Type        string         `json:"type" binding:"required"`
	Description string         `json:"description,omitempty"`
	Values      map[string]any `json:"values" binding:"required"`
	// ExistingSecret names a Secret already present in the namespace, holding
	// the credentials of this connection. When it is set the server stores no
	// credential of its own: the Secret may come from anywhere, typically
	// projected from a vault by External Secrets, and the connection only
	// points at it. The credential fields of the payload are then ignored.
	ExistingSecret string `json:"existingSecret,omitempty"`
}

// ConnectionTestRequest is the body of a connectivity test. It carries no name:
// a connection is tested before it is created.
type ConnectionTestRequest struct {
	Type   string         `json:"type" binding:"required"`
	Values map[string]any `json:"values" binding:"required"`
}

// ConnectionTestResult is the outcome of a connectivity test. Success is the
// verdict that matters for a deployment: the path a workload will take. Checks
// details every path that was probed, because a store reachable from outside
// the cluster and unreachable from inside it is a real and confusing case.
type ConnectionTestResult struct {
	Success    bool                  `json:"success"`
	Message    string                `json:"message"`
	Reason     string                `json:"reason,omitempty"`
	DurationMs int64                 `json:"durationMs"`
	Checks     []ConnectionTestCheck `json:"checks,omitempty"`
}

// ConnectionTestCheck is one probed path.
type ConnectionTestCheck struct {
	// Label names the path for a reader ("In-cluster URL", "Public URL").
	Label  string `json:"label"`
	Target string `json:"target"`
	// Decisive marks the path a workload will actually take, the one Success
	// reflects. The others are shown for diagnosis.
	Decisive bool   `json:"decisive"`
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Reason   string `json:"reason,omitempty"`
}

// CredentialsSecretRef is the Secret holding a connection's credentials, with
// the keys it carries. It names no single key because a connection may hold
// several.
type CredentialsSecretRef struct {
	Name      string   `json:"name"`
	Namespace string   `json:"namespace,omitempty"`
	Keys      []string `json:"keys,omitempty"`
	// Owned reports that the console wrote this Secret, as opposed to pointing
	// at one that was already there, typically projected from a vault. The two
	// behave differently on edit and on delete, so the panel says which it is.
	Owned bool `json:"owned"`
}

// ConnectionResponse is an external connection as returned by the API. The
// values of secret fields are never included. Only their names are, so that
// the console can show a credential as set without being able to read it.
type ConnectionResponse struct {
	Name         string         `json:"name"`
	Type         string         `json:"type"`
	Scope        string         `json:"scope"`
	Namespace    string         `json:"namespace,omitempty"`
	Description  string         `json:"description,omitempty"`
	Status       string         `json:"status"`
	Message      string         `json:"message,omitempty"`
	Values       map[string]any `json:"values"`
	SecretFields []string       `json:"secretFields,omitempty"`
	// CredentialsSecret is the Secret holding the secret fields, so a consumer
	// can reference it from a Deployment without the console ever reading it.
	CredentialsSecret *CredentialsSecretRef `json:"credentialsSecret,omitempty"`
	CreatedAt         string                `json:"createdAt,omitempty"`
}

// InternalConnection is a connection provided by a service already deployed in
// the project, that the project's other services can consume.
type InternalConnection struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	TypeDisplay string `json:"typeDisplay"`
	Icon        string `json:"icon,omitempty"`
	Category    string `json:"category,omitempty"`
	// Service and ReleaseName identify the deployed instance providing it.
	Service     string `json:"service"`
	ReleaseName string `json:"releaseName"`
	Namespace   string `json:"namespace"`
	Status      string `json:"status"`
	// Endpoint is the in-cluster address another service uses to reach it.
	Endpoint string         `json:"endpoint"`
	Host     string         `json:"host"`
	Port     int32          `json:"port"`
	Values   map[string]any `json:"values"`
	// Managed reports that the entry comes from a Connection the KuboCD release
	// controller owns, rather than being derived from the deployed service.
	Managed   bool   `json:"managed"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// PackageInput is one connection a package declares it needs. The console
// offers a choice for each: an existing connection, a new one, or nothing at
// all when the package marks it optional.
type PackageInput struct {
	// Alias names the input in the package's templates, and is the key the
	// console sends back when the user picks a connection.
	Alias string `json:"alias"`
	// Contract names the contract the chosen connection must satisfy. It is what
	// makes the choice safe: only connections of that contract are offered.
	Contract string `json:"contract"`
	// Parameter is the package parameter carrying the chosen connection name,
	// derived from the input's namedConnection template. Empty when the input
	// binds some other way, in which case the console offers no choice.
	Parameter string `json:"parameter,omitempty"`
	// Optional reports that the package tolerates no connection at all.
	Optional bool `json:"optional"`
	// Default is the template the package falls back to when the deployer picks
	// nothing. KuboCD forbids a literal here, so it is always rendered against
	// the Context: this is how an Environment says "here, the database is that
	// one" without the deployer naming it. The console must therefore leave the
	// parameter out rather than send an empty string, which would win over it.
	Default string `json:"default,omitempty"`
	// Description is shown next to the field.
	Description string `json:"description,omitempty"`
}

// SelectableConnection is a connection offered when deploying a service whose
// package declares an input, just what the picker needs to render a choice.
// Managed connections are included: a Trino published by another release is
// exactly the kind of thing a new service wants to bind.
type SelectableConnection struct {
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Description string `json:"description,omitempty"`
	Managed     bool   `json:"managed"`
	// ProvidedBy names the release publishing a managed connection.
	ProvidedBy string `json:"providedBy,omitempty"`
}

// ConnectionConsumer is one service bound to a connection. The console shows
// these before a destructive edit: deleting a connection takes its Secret with
// it, and the pods mounting that Secret fail at their next restart with
// CreateContainerConfigError, far from the dialog that caused it.
type ConnectionConsumer struct {
	// Service is the instance name as the console displays it, ReleaseName the
	// underlying KuboCD Release.
	Service     string `json:"service"`
	ReleaseName string `json:"releaseName"`
	Status      string `json:"status"`
	// Effective reports that the release actually resolved this connection, as
	// opposed to merely waiting for it.
	Effective bool `json:"effective"`
}
