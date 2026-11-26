// Package engine defines the ports between the control plane core and a
// deployment engine. The core (handlers, validation, authentication, pods and
// metrics) reaches an engine only through these four ports, so that the engine
// can be KuboCD writing to the cluster or another one committing to git.
package engine

import (
	"context"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// Engine bundles the four ports of one deployment engine with what it says
// about itself.
type Engine struct {
	Info           Info
	DesiredState   DesiredState
	Observer       Observer
	Catalog        Catalog
	PlatformConfig PlatformConfig
}

// Info describes an engine to the core and, through the capabilities route,
// to the console.
type Info struct {
	// Name is for display only. The console tests capabilities, never the name.
	Name        string
	Persistence Persistence
	// RenderedValues reports that the engine can show the values it rendered
	// for an instance.
	RenderedValues bool
	// Suspend reports that an instance can be suspended through the console.
	Suspend bool
}

// Persistence modes.
const (
	PersistenceKubernetes = "kubernetes"
	PersistenceGit        = "git"
)

// Persistence describes where the engine stores the desired state.
type Persistence struct {
	// Mode is PersistenceKubernetes or PersistenceGit.
	Mode string
}

// InstanceRef names a deployed instance. Name is the instance name the user
// chose, not the name of whatever object the engine stores it in.
type InstanceRef struct {
	Project string
	Name    string
}

// Instance is the desired state of a deployed instance.
type Instance struct {
	Ref        InstanceRef
	Service    string
	Version    string
	Parameters map[string]any
}

// Phase is the state of an instance in the vocabulary shared by all engines.
type Phase string

const (
	// PhasePending: the write was accepted, the engine has not acted on it yet.
	PhasePending    Phase = "Pending"
	PhaseInstalling Phase = "Installing"
	PhaseUpdating   Phase = "Updating"
	PhaseReady      Phase = "Ready"
	// PhaseDegraded: the engine reports the instance ready but its workloads are not healthy.
	PhaseDegraded  Phase = "Degraded"
	PhaseError     Phase = "Error"
	PhaseSuspended Phase = "Suspended"
	PhaseDeleting  Phase = "Deleting"
	// PhaseUnknown: the instance could not be observed. Never a synonym of Pending.
	PhaseUnknown Phase = "Unknown"
)

// InstanceStatus is what an engine observed about an instance.
type InstanceStatus struct {
	// Phase ignores a pending deletion, which Deleting reports.
	Phase Phase
	// EnginePhase is the engine's own word for the state, kept for display
	// (KuboCD: READY, WAIT_HREL...).
	EnginePhase string
	// Deleting reports a deletion requested and not finished yet.
	Deleting bool
	Message  string
	// The three revisions are opaque and empty when the engine cannot tell.
	// DesiredRevision is the stored desired state (KuboCD: metadata.generation,
	// git: the commit), PublishedRevision what a build step produced from it,
	// AppliedRevision what the engine proved it applied.
	DesiredRevision   string
	PublishedRevision string
	AppliedRevision   string
}

// ConnectionBinding is one connection an instance asked for, as the engine
// reports it. An instance may list several candidates of one name, of
// different kinds, and only some of them bound.
type ConnectionBinding struct {
	Kind      string
	Name      string
	Namespace string
	// Bound is false for a candidate the engine watches but did not resolve.
	Bound bool
	// State is how far the connection got, empty when the engine cannot tell
	// or the connection does not exist.
	State ConnectionState
}

// ConnectionState is how far a connection got, seen from an instance that
// consumes it. The states are ordered.
type ConnectionState string

const (
	// ConnectionDeclared: the connection exists, declared by a user.
	ConnectionDeclared ConnectionState = "declared"
	// ConnectionPublished: the connection exists, published by the instance providing it.
	ConnectionPublished ConnectionState = "published"
	// ConnectionBound: the consuming instance is bound to it.
	ConnectionBound ConnectionState = "bound"
	// ConnectionReady: bound, and the instance providing it is ready.
	ConnectionReady ConnectionState = "ready"
)

// Rank orders the states, 0 for an empty one.
func (s ConnectionState) Rank() int {
	switch s {
	case ConnectionDeclared:
		return 1
	case ConnectionPublished:
		return 2
	case ConnectionBound:
		return 3
	case ConnectionReady:
		return 4
	default:
		return 0
	}
}

// InstanceView is an instance as the engine observes it: desired state and status.
type InstanceView struct {
	Ref InstanceRef
	// EngineName names the object the engine stores the instance in (KuboCD:
	// the Release). OIDC clients and the objects an instance leaves behind are
	// named after it.
	EngineName      string
	Service         string
	Version         string
	Parameters      map[string]any
	TargetNamespace string
	Roles           []string
	Bindings        []ConnectionBinding
	CreatedAt       time.Time
	Status          InstanceStatus
}

// EventType mirrors the Kubernetes watch event types the console already reads.
type EventType string

const (
	EventAdded    EventType = "ADDED"
	EventModified EventType = "MODIFIED"
	EventDeleted  EventType = "DELETED"
)

// InstanceEvent is one change of an instance.
type InstanceEvent struct {
	Type     EventType
	Instance InstanceView
}

// Workloads identifies what an instance runs.
type Workloads struct {
	// Names are the app.kubernetes.io/instance values of the instance's workloads.
	Names []string
}

// DesiredState writes and reads the desired state.
//
// A write returns an error only when it could not be attempted or failed for
// a reason unrelated to its content, and the Operation is then meaningless.
// Otherwise the Operation outcome says what happened. A missing instance is
// reported by an error wrapping ErrNotFound.
type DesiredState interface {
	CreateInstance(ctx context.Context, in Instance, opts WriteOptions) (Operation, error)
	// UpdateInstance replaces the desired state of an existing instance with in.
	UpdateInstance(ctx context.Context, in Instance, opts WriteOptions) (Operation, error)
	DeleteInstance(ctx context.Context, ref InstanceRef, opts WriteOptions) (Operation, error)
	// GetInstance returns the desired state and the revision to pass as
	// WriteOptions.ExpectedRevision.
	GetInstance(ctx context.Context, ref InstanceRef) (Instance, string, error)
	ListInstances(ctx context.Context, project string) ([]Instance, error)

	// Catalog entries. The core checks that the entry exists, or not, before
	// calling. The engine checks what only it can, such as published versions.
	CreateCatalogEntry(ctx context.Context, svc models.PlatformService, opts WriteOptions) (Operation, error)
	UpdateCatalogEntry(ctx context.Context, svc models.PlatformService, opts WriteOptions) (Operation, error)
	DeleteCatalogEntry(ctx context.Context, name string, opts WriteOptions) (Operation, error)
}

// Observer reads what the engine observed.
type Observer interface {
	ObserveInstance(ctx context.Context, ref InstanceRef) (InstanceView, error)
	ObserveInstances(ctx context.Context, project string) ([]InstanceView, error)
	// EngineName is the name of the object the engine stores ref in.
	EngineName(ref InstanceRef) string
	// Workloads identifies what the engine object engineName, in namespace,
	// runs. The core looks workloads up by engine name because an instance
	// listed by the engine is known by that name.
	Workloads(ctx context.Context, namespace, engineName string) (Workloads, error)
	// WatchInstances streams the changes of a project's instances. The channel
	// is closed when ctx ends or when the engine stops the stream, after which
	// the caller opens a new one.
	WatchInstances(ctx context.Context, project string) (<-chan InstanceEvent, error)
}

// Catalog reads the services an engine can deploy.
type Catalog interface {
	// Entries returns the services as configured, without asking any registry.
	Entries(ctx context.Context) ([]models.PlatformService, error)
	// Services returns the services with the versions actually published.
	Services(ctx context.Context) ([]models.PlatformService, error)
	Categories(ctx context.Context) ([]models.MenuCategory, error)
	Versions(ctx context.Context, service string) (*models.ServiceVersions, error)
	// ParameterSchema returns the JSON Schema of a version's parameters, in the
	// dialect the console reads.
	ParameterSchema(ctx context.Context, service, version string) (map[string]any, error)
	Inputs(ctx context.Context, service, version string) ([]models.PackageInput, error)
}

// PlatformConfig reads the platform settings the engine stores.
type PlatformConfig interface {
	IngressSuffix(ctx context.Context) (string, error)
	ProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error)
	SparkConfig(ctx context.Context) (*models.SparkConfig, error)
}
