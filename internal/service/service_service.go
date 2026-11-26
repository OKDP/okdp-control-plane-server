package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Catalog management errors. Handlers map these to HTTP 400/404/409.
var (
	ErrCatalogValidation      = errors.New("invalid catalog service")
	ErrCatalogServiceExists   = errors.New("service already exists in the catalog")
	ErrCatalogServiceNotFound = errors.New("service not found in the catalog")
)

// ServiceService manages platform services (deploy, monitor, delete) and exposes the catalog.
type ServiceService interface {
	GetPlatformServices(ctx context.Context) ([]models.PlatformService, error)
	// AddPlatformService exposes a new service in the catalog (default Context).
	AddPlatformService(ctx context.Context, svc models.PlatformService) (*models.PlatformService, error)
	// UpdatePlatformService updates an existing catalog service (name from the path).
	UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) (*models.PlatformService, error)
	// RemovePlatformService removes a service from the catalog.
	RemovePlatformService(ctx context.Context, name string) error
	DeployService(ctx context.Context, project string, req models.ServiceRequest) (*models.ServiceInstance, error)
	ListServices(ctx context.Context, project string) ([]models.ServiceInstance, error)
	GetService(ctx context.Context, project, name string) (*models.ServiceInstance, error)
	// UpdateServiceParameters and DeleteService refuse the write as a conflict
	// when expectedRevision is set and the instance is no longer at it.
	UpdateServiceParameters(ctx context.Context, project, name string, req models.ServiceUpdateRequest, expectedRevision string) (*models.ServiceInstance, error)
	// With git persistence the deletion is only committed and the answer
	// carries the operation (accepted, with its desired revision). It is nil
	// when the deletion is done in the cluster.
	DeleteService(ctx context.Context, project, name, expectedRevision string) (*models.Operation, error)
	// WatchServices streams the changes of a project's services. The channel
	// closes when ctx ends or the engine stops the stream.
	WatchServices(ctx context.Context, project string) (<-chan ServiceEvent, error)

	GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error)

	GetIngressSuffix(ctx context.Context) (string, error)

	GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error)

	ListPods(ctx context.Context, project, serviceName string) ([]models.Pod, error)
	GetPodLogs(ctx context.Context, project, podName, container string, tailLines int64, follow bool) (io.ReadCloser, error)
	GetServiceMetrics(ctx context.Context, project, serviceName string) (*models.ServiceMetrics, error)
	GetProjectMetrics(ctx context.Context, project string) (map[string]*models.ServiceMetrics, error)
}

// ServiceEvent is one change of a service, as the services stream sends it.
type ServiceEvent struct {
	Type     string
	Instance models.ServiceInstance
}

// DefaultServiceService is the engine-independent part of service management.
// The engine stores and observes the instances. This service validates what is
// written, and reads pods, Ingresses, events and metrics from the cluster.
type DefaultServiceService struct {
	engine          engine.Engine
	oidcProvisioner provisioning.OidcClientProvisioner
	k8sClient       dynamic.Interface
	typedClient     kubernetes.Interface
	sidecarPrefixes []string
}

func NewDefaultServiceService(eng engine.Engine, oidcProvisioner provisioning.OidcClientProvisioner, k8sClient dynamic.Interface, typedClient kubernetes.Interface, sidecarPrefixes []string) *DefaultServiceService {
	return &DefaultServiceService{
		engine:          eng,
		oidcProvisioner: oidcProvisioner,
		k8sClient:       k8sClient,
		typedClient:     typedClient,
		sidecarPrefixes: sidecarPrefixes,
	}
}

// --- Platform services ---

func (s *DefaultServiceService) GetPlatformServices(ctx context.Context) ([]models.PlatformService, error) {
	return s.engine.Catalog.Services(ctx)
}

// AddPlatformService validates and appends a new service to the catalog (default Context).
func (s *DefaultServiceService) AddPlatformService(ctx context.Context, svc models.PlatformService) (*models.PlatformService, error) {
	if err := normalizeAndValidateCatalogService(&svc); err != nil {
		return nil, err
	}

	existing, err := s.engine.Catalog.Entries(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read catalog: %w", err)
	}
	for _, e := range existing {
		if e.Name == svc.Name {
			return nil, fmt.Errorf("%w: %q", ErrCatalogServiceExists, svc.Name)
		}
	}

	op, err := s.engine.DesiredState.CreateCatalogEntry(ctx, svc, engine.WriteOptions{})
	if err := catalogWriteError(op, err); err != nil {
		return nil, err
	}
	return &svc, nil
}

// catalogWriteError turns the answer to a catalog write into the error the
// handlers map to a status code, nil when the write was accepted.
func catalogWriteError(op engine.Operation, err error) error {
	if err != nil {
		return err
	}
	switch op.Outcome {
	case engine.OutcomeAccepted:
		return nil
	case engine.OutcomeRejected:
		rejected := &engine.RejectedError{Operation: op}
		if rejected.HasCode(engine.CodeInvalidCatalogEntry) {
			return fmt.Errorf("%w: %s", ErrCatalogValidation, rejected.Error())
		}
		return rejected
	default:
		return &engine.UnknownError{Operation: op}
	}
}

// UpdatePlatformService validates and replaces an existing catalog service.
func (s *DefaultServiceService) UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) (*models.PlatformService, error) {
	svc.Name = name // the path is the source of truth for identity
	if err := normalizeAndValidateCatalogService(&svc); err != nil {
		return nil, err
	}

	if _, err := s.findCatalogService(ctx, name); err != nil {
		return nil, err
	}

	op, err := s.engine.DesiredState.UpdateCatalogEntry(ctx, svc, engine.WriteOptions{})
	if err := catalogWriteError(op, err); err != nil {
		return nil, err
	}
	return &svc, nil
}

// RemovePlatformService removes a service from the catalog.
func (s *DefaultServiceService) RemovePlatformService(ctx context.Context, name string) error {
	if _, err := s.findCatalogService(ctx, name); err != nil {
		return err
	}
	op, err := s.engine.DesiredState.DeleteCatalogEntry(ctx, name, engine.WriteOptions{})
	return catalogWriteError(op, err)
}

// findCatalogService returns the catalog service by name or ErrCatalogServiceNotFound.
func (s *DefaultServiceService) findCatalogService(ctx context.Context, name string) (*models.PlatformService, error) {
	existing, err := s.engine.Catalog.Entries(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read catalog: %w", err)
	}
	for i := range existing {
		if existing[i].Name == name {
			return &existing[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrCatalogServiceNotFound, name)
}

// normalizeAndValidateCatalogService enforces the catalog rules and fills the
// default version when omitted. Returns an error wrapping ErrCatalogValidation.
func normalizeAndValidateCatalogService(svc *models.PlatformService) error {
	svc.Name = strings.TrimSpace(svc.Name)
	if svc.Name == "" {
		return fmt.Errorf("%w: name is required", ErrCatalogValidation)
	}
	if errs := validation.IsDNS1123Label(svc.Name); len(errs) > 0 {
		return fmt.Errorf("%w: name %q must be a valid DNS-1123 label: %s", ErrCatalogValidation, svc.Name, errs[0])
	}
	svc.Repository = strings.TrimSpace(svc.Repository)
	if svc.Repository != "" {
		if strings.Contains(svc.Repository, "://") || strings.ContainsAny(svc.Repository, "@?# \t\r\n") {
			return fmt.Errorf("%w: repository must be a bare OCI repository reference without scheme, credentials, query, fragment, or whitespace", ErrCatalogValidation)
		}
	}
	svc.Category = strings.TrimSpace(svc.Category)
	if svc.Category == "" {
		return fmt.Errorf("%w: category is required, it names the console section the service belongs to", ErrCatalogValidation)
	}
	if len(svc.Versions) == 0 {
		return fmt.Errorf("%w: at least one version is required", ErrCatalogValidation)
	}
	seen := make(map[string]bool, len(svc.Versions))
	for _, v := range svc.Versions {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%w: version cannot be empty", ErrCatalogValidation)
		}
		if seen[v] {
			return fmt.Errorf("%w: duplicate version %q", ErrCatalogValidation, v)
		}
		seen[v] = true
	}
	if svc.DefaultVersion == "" {
		svc.DefaultVersion = svc.Versions[0]
	} else if !seen[svc.DefaultVersion] {
		return fmt.Errorf("%w: default version %q must be one of the listed versions", ErrCatalogValidation, svc.DefaultVersion)
	}
	return nil
}

func (s *DefaultServiceService) DeployService(ctx context.Context, project string, req models.ServiceRequest) (*models.ServiceInstance, error) {
	if req.Service == "" {
		return nil, fmt.Errorf("service name is required")
	}

	svc, err := s.resolvePlatformService(ctx, req.Service)
	if err != nil {
		return nil, err
	}

	deployTag := req.Tag
	if deployTag == "" {
		deployTag = svc.DefaultVersion
	}

	if err := s.validateParameters(ctx, req.Service, deployTag, req.Parameters); err != nil {
		return nil, &ParameterError{err: err}
	}

	ingressSuffix, err := s.engine.PlatformConfig.IngressSuffix(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve ingress suffix: %w", err)
	}

	instanceName := req.InstanceName
	if instanceName == "" {
		instanceName = req.Service
	}
	desired := engine.Instance{
		Ref:        engine.InstanceRef{Project: project, Name: instanceName},
		Service:    req.Service,
		Version:    deployTag,
		Parameters: req.Parameters,
	}

	op, err := s.engine.DesiredState.CreateInstance(ctx, desired, engine.WriteOptions{})
	if err := writeError(op, err); err != nil {
		return nil, err
	}
	if op.Instance == nil {
		return submittedInstance(desired, op), nil
	}

	instance := baseInstance(*op.Instance)
	instance.URL = fmt.Sprintf("https://%s.%s", op.Instance.EngineName, ingressSuffix)
	instance.Operation = OperationOf(op)
	return instance, nil
}

// ParameterError reports a failed parameter validation: parameters the
// schema refuses, or a schema that could not be fetched.
type ParameterError struct{ err error }

func (e *ParameterError) Error() string { return "parameter validation failed: " + e.err.Error() }
func (e *ParameterError) Unwrap() error { return e.err }

// Rejection is the operation of a write refused because the schema refuses
// its parameters. False when the validation could not run.
func (e *ParameterError) Rejection() (engine.Operation, bool) {
	var violation *schemaViolation
	if !errors.As(e.err, &violation) {
		return engine.Operation{}, false
	}
	return engine.Rejected(engine.CodeInvalidParameters, violation.Error()), true
}

// schemaViolation is a parameter set the service's schema refuses.
type schemaViolation struct{ err error }

func (e *schemaViolation) Error() string { return "invalid parameters: " + e.err.Error() }
func (e *schemaViolation) Unwrap() error { return e.err }

// OperationOf describes an engine operation in the API model.
func OperationOf(op engine.Operation) *models.Operation {
	out := &models.Operation{Outcome: string(op.Outcome), DesiredRevision: op.DesiredRevision}
	for _, p := range op.Problems {
		out.Problems = append(out.Problems, models.Problem{Code: p.Code, Field: p.Field, Message: p.Message})
	}
	return out
}

// writeError turns the answer to an instance write into an error, nil when
// the write was accepted.
func writeError(op engine.Operation, err error) error {
	if err != nil {
		return err
	}
	switch op.Outcome {
	case engine.OutcomeAccepted:
		return nil
	case engine.OutcomeRejected:
		return &engine.RejectedError{Operation: op}
	default:
		return &engine.UnknownError{Operation: op}
	}
}

// submittedInstance describes an accepted write the engine has not described
// back: what was asked, not yet observed.
func submittedInstance(in engine.Instance, op engine.Operation) *models.ServiceInstance {
	return &models.ServiceInstance{
		Name:            in.Ref.Name,
		Service:         in.Service,
		ServiceTag:      in.Version,
		Status:          LegacyStatus(engine.PhasePending),
		TargetNamespace: in.Ref.Project,
		Parameters:      in.Parameters,
		Phase:           string(engine.PhasePending),
		DesiredRevision: op.DesiredRevision,
		Operation:       OperationOf(op),
	}
}

func (s *DefaultServiceService) ListServices(ctx context.Context, project string) ([]models.ServiceInstance, error) {
	views, err := s.engine.Observer.ObserveInstances(ctx, project)
	if err != nil {
		return nil, err
	}

	var result []models.ServiceInstance
	for i := range views {
		result = append(result, *baseInstance(views[i]))
	}
	s.enrichWithURL(ctx, result)
	s.enrichWithPodHealth(ctx, result)
	return result, nil
}

func (s *DefaultServiceService) GetService(ctx context.Context, project, name string) (*models.ServiceInstance, error) {
	view, err := s.engine.Observer.ObserveInstance(ctx, engine.InstanceRef{Project: project, Name: name})
	if err != nil {
		return nil, err
	}
	instance := baseInstance(view)
	s.setURLIfExposed(ctx, instance)
	instances := []models.ServiceInstance{*instance}
	s.enrichWithPodHealth(ctx, instances)
	instance.Status = instances[0].Status
	instance.Phase = instances[0].Phase
	// Only surface an explanatory message when the instance is not Ready: there
	// is nothing useful to show on a healthy service, and scanning events has a
	// non-trivial API cost per request.
	if instance.Status != "Ready" {
		if warn := s.latestWarningMessage(ctx, project, instance.ReleaseName); warn != "" {
			instance.StatusMessage = warn
		}
	}
	return instance, nil
}

// latestWarningMessage scans Warning events in a namespace and returns the
// most recent message whose involvedObject name is related to the given release.
func (s *DefaultServiceService) latestWarningMessage(ctx context.Context, project, releaseName string) string {
	if project == "" || releaseName == "" {
		return ""
	}
	names, err := s.workloadNames(ctx, project, releaseName)
	if err != nil {
		logrus.WithError(err).Debugf("latestWarningMessage: instance names failed for %s", releaseName)
		return ""
	}

	eventsGVR := schema.GroupVersionResource{Version: "v1", Resource: "events"}
	events, err := s.k8sClient.Resource(eventsGVR).Namespace(project).List(ctx, metav1.ListOptions{
		FieldSelector: "type=Warning",
	})
	if err != nil {
		logrus.WithError(err).Debugf("latestWarningMessage: event list failed in %s", project)
		return ""
	}

	var (
		latestAt  time.Time
		latestMsg string
	)
	for _, e := range events.Items {
		involvedName, _, _ := unstructured.NestedString(e.Object, "involvedObject", "name")
		if !matchesInstance(involvedName, names) {
			continue
		}
		ts := pickEventTimestamp(e.Object)
		if ts.After(latestAt) {
			latestAt = ts
			msg, _, _ := unstructured.NestedString(e.Object, "message")
			latestMsg = truncateMessage(msg)
		}
	}
	return latestMsg
}

// pickEventTimestamp reads the most useful timestamp from an Event object.
// Modern events populate eventTime; legacy events use lastTimestamp. Fall
// back to metadata.creationTimestamp as a last resort.
func pickEventTimestamp(obj map[string]interface{}) time.Time {
	for _, path := range [][]string{
		{"lastTimestamp"},
		{"eventTime"},
		{"metadata", "creationTimestamp"},
	} {
		if s, _, _ := unstructured.NestedString(obj, path...); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// truncateMessage keeps UI tooltips readable. K8s validation errors can run
// several hundred characters, we cap to 400 so the UI does not blow up.
func truncateMessage(msg string) string {
	const max = 400
	if len(msg) <= max {
		return msg
	}
	return msg[:max] + "…"
}

func (s *DefaultServiceService) DeleteService(ctx context.Context, project, name, expectedRevision string) (*models.Operation, error) {
	ref := engine.InstanceRef{Project: project, Name: name}
	op, err := s.engine.DesiredState.DeleteInstance(ctx, ref, engine.WriteOptions{ExpectedRevision: expectedRevision})
	if err := writeError(op, err); err != nil {
		return nil, err
	}

	releaseName := s.engine.Observer.EngineName(ref)
	if op.Instance != nil && op.Instance.EngineName != "" {
		releaseName = op.Instance.EngineName
	}
	// With git persistence the deletion is only committed: the workloads still
	// run until the engine applies it, and their volumes must outlive them.
	if s.engine.Info.Persistence.Mode != engine.PersistenceKubernetes {
		logrus.WithField("instance", releaseName).Info("Deletion committed, OIDC client and user resources are left in place")
		return OperationOf(op), nil
	}

	s.cleanupOidcClient(ctx, releaseName)
	s.cleanupUserResources(ctx, project, releaseName)
	return nil, nil
}

// cleanupOidcClient best-effort unregisters the OIDC client of a deleted
// service through the configured provisioning backend (no-op when
// identity.provisioning.provider is unset/none).
func (s *DefaultServiceService) cleanupOidcClient(ctx context.Context, releaseName string) {
	if err := s.oidcProvisioner.DeleteClient(ctx, releaseName); err != nil {
		// Warn, not Debug: a client left registered outlives the service that
		// owned it and nothing else reports it.
		logrus.WithError(err).WithField("oidcClient", releaseName).Warn("OidcClient cleanup failed")
	} else {
		logrus.WithField("oidcClient", releaseName).Info("Cleaned up OidcClient")
	}
}

// ownsByName reports whether an object named by a release belongs to
// releaseName rather than to one of the other releases still living in the
// namespace. Names are the only link available here (the singleuser objects
// JupyterHub creates carry no ownerReference back to the Release), and a bare
// prefix test is not enough: `demo-jupyter-` also matches
// `demo-jupyter-ds-claim-alice`, the home volume of a user of the *other*
// instance. Whichever prefix is the longest match wins, so an object is only
// ever claimed by the release whose name reaches furthest into it.
func ownsByName(objectName, releaseName string, otherReleases []string) bool {
	prefix := releaseName + "-"
	if len(objectName) <= len(prefix) || objectName[:len(prefix)] != prefix {
		return false
	}
	for _, other := range otherReleases {
		if len(other) <= len(releaseName) {
			continue
		}
		// The neighbour's own name is one of its objects too: a chart names its
		// PVC after the release itself.
		if objectName == other {
			return false
		}
		otherPrefix := other + "-"
		if len(objectName) > len(otherPrefix) && objectName[:len(otherPrefix)] == otherPrefix {
			return false
		}
	}
	return true
}

// cleanupUserResources removes the pods and volumes a release created outside
// its own manifests, JupyterHub's per-user servers and home volumes, which no
// controller reclaims when the Release goes.
//
// Deleting a volume is irreversible, so this fails closed: if the surviving
// releases cannot be listed, nothing is deleted rather than everything matching
// a prefix.
func (s *DefaultServiceService) cleanupUserResources(ctx context.Context, namespace, releaseName string) {
	// Called after the Release has been deleted, so this lists the neighbours.
	survivors, err := s.engine.Observer.ObserveInstances(ctx, namespace)
	if err != nil {
		logrus.WithError(err).WithField("release", releaseName).
			Error("Skipping user resource cleanup: cannot tell this release's objects from its neighbours'")
		return
	}
	others := make([]string, 0, len(survivors))
	for i := range survivors {
		if survivors[i].EngineName != releaseName {
			others = append(others, survivors[i].EngineName)
		}
	}

	for _, target := range []struct {
		gvr  schema.GroupVersionResource
		kind string
	}{
		{schema.GroupVersionResource{Version: "v1", Resource: "pods"}, "pod"},
		{schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}, "pvc"},
	} {
		objects, err := s.k8sClient.Resource(target.gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			logrus.WithError(err).WithField("kind", target.kind).Warn("Could not list user resources to clean up")
			continue
		}
		for _, object := range objects.Items {
			if !ownsByName(object.GetName(), releaseName, others) {
				continue
			}
			if err := s.k8sClient.Resource(target.gvr).Namespace(namespace).Delete(ctx, object.GetName(), metav1.DeleteOptions{}); err != nil {
				logrus.WithError(err).WithField(target.kind, object.GetName()).Warn("Could not clean up user resource")
				continue
			}
			logrus.WithField(target.kind, object.GetName()).Info("Cleaned up user resource")
		}
	}
}

func (s *DefaultServiceService) WatchServices(ctx context.Context, project string) (<-chan ServiceEvent, error) {
	events, err := s.engine.Observer.WatchInstances(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make(chan ServiceEvent)
	go func() {
		defer close(out)
		for event := range events {
			instance := baseInstance(event.Instance)
			// The stream sends no connections.
			instance.Connections = nil
			s.setURLIfExposed(ctx, instance)
			s.enrichPodHealth(ctx, instance)
			select {
			case out <- ServiceEvent{Type: string(event.Type), Instance: *instance}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// --- Catalog (self-service) ---

func (s *DefaultServiceService) GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error) {
	return s.engine.Catalog.Categories(ctx)
}

func (s *DefaultServiceService) GetIngressSuffix(ctx context.Context) (string, error) {
	return s.engine.PlatformConfig.IngressSuffix(ctx)
}

func (s *DefaultServiceService) GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error) {
	return s.engine.PlatformConfig.ProfileImages(ctx)
}

// UpdateServiceParameters merges the submitted parameters into the current ones,
// each top-level key replacing the stored one, validates the result against
// the schema of the resulting version, and stores it. When expectedRevision is
// empty, the write still checks the revision read and retries the read, merge
// and validation on conflict so concurrent updates are never silently lost.
func (s *DefaultServiceService) UpdateServiceParameters(ctx context.Context, project, name string, req models.ServiceUpdateRequest, expectedRevision string) (*models.ServiceInstance, error) {
	const maxPatchCASAttempts = 5
	ref := engine.InstanceRef{Project: project, Name: name}
	for attempt := 0; ; attempt++ {
		desired, revision, err := s.engine.DesiredState.GetInstance(ctx, ref)
		if err != nil {
			return nil, err
		}
		if expectedRevision != "" && expectedRevision != revision {
			return nil, &engine.RejectedError{Operation: engine.Rejected(engine.CodeConflict,
				fmt.Sprintf("instance %q changed since revision %s", name, expectedRevision))}
		}

		if req.Tag != "" {
			desired.Version = req.Tag
		}

		if req.Parameters != nil {
			merged := make(map[string]any, len(desired.Parameters)+len(req.Parameters))
			for k, v := range desired.Parameters {
				merged[k] = v
			}
			for k, v := range req.Parameters {
				merged[k] = v
			}
			desired.Parameters = merged
		}

		if err := s.validateParameters(ctx, desired.Service, desired.Version, desired.Parameters); err != nil {
			return nil, &ParameterError{err: err}
		}

		writeRevision := expectedRevision
		if writeRevision == "" {
			writeRevision = revision
		}
		op, err := s.engine.DesiredState.UpdateInstance(ctx, desired, engine.WriteOptions{ExpectedRevision: writeRevision})
		if err == nil && expectedRevision == "" && attempt < maxPatchCASAttempts-1 &&
			op.Outcome == engine.OutcomeRejected && len(op.Problems) > 0 && op.Problems[0].Code == engine.CodeConflict {
			continue
		}
		if err := writeError(op, err); err != nil {
			return nil, err
		}
		if op.Instance == nil {
			return submittedInstance(desired, op), nil
		}

		instance := baseInstance(*op.Instance)
		s.setURLIfExposed(ctx, instance)
		instance.Operation = OperationOf(op)
		return instance, nil
	}
}

// --- helpers ---

func (s *DefaultServiceService) enrichWithURL(ctx context.Context, instances []models.ServiceInstance) {
	suffix, err := s.engine.PlatformConfig.IngressSuffix(ctx)
	if err != nil {
		return
	}
	// Get the URL only if the Ingress belongs to this instance.
	// This avoids matching an Ingress from another instance with a similar name,
	// such as "kafka" and "kafka-console".
	// Ingresses are loaded once per namespace and reused by all instances.
	hostsByNS := map[string]map[string]string{}
	for i := range instances {
		ns := instances[i].TargetNamespace
		if ns == "" {
			continue
		}
		hosts, ok := hostsByNS[ns]
		if !ok {
			hosts = s.namespaceIngressHosts(ctx, ns)
			hostsByNS[ns] = hosts
		}
		names, err := s.workloadNames(ctx, ns, instances[i].ReleaseName)
		if err != nil {
			continue
		}
		for _, host := range candidateHosts(&instances[i], suffix) {
			if owner, exists := hosts[host]; exists && slices.Contains(names, owner) {
				instances[i].URL = "https://" + host
				break
			}
		}
	}
}

// setURLIfExposed sets instance.URL to the first candidate host (see
// candidateHosts) that an Ingress in the instance's namespace actually
// routes to, the single-instance counterpart of enrichWithURL (see it for
// the rationale).
func (s *DefaultServiceService) setURLIfExposed(ctx context.Context, instance *models.ServiceInstance) {
	suffix, err := s.engine.PlatformConfig.IngressSuffix(ctx)
	if err != nil || instance == nil || instance.TargetNamespace == "" {
		return
	}
	names, err := s.workloadNames(ctx, instance.TargetNamespace, instance.ReleaseName)
	if err != nil {
		return
	}
	hosts := s.namespaceIngressHosts(ctx, instance.TargetNamespace)
	for _, host := range candidateHosts(instance, suffix) {
		if owner, exists := hosts[host]; exists && slices.Contains(names, owner) {
			instance.URL = "https://" + host
			return
		}
	}
}

// roleHostConventions maps a role (models.ServiceInstance.Roles) to the
// canonical Ingress host a service filling that role is expected to publish,
// for roles whose URL is a project-wide convention rather than tied to
// whatever name a given instance happens to have, e.g. a project has at
// most one default storage service, so consumers and the console can assume
// a single, stable host regardless of which backend/instance is deployed.
var roleHostConventions = map[string]func(namespace string) string{
	"storage": func(namespace string) string { return fmt.Sprintf("storage-%s", namespace) },
	// The Spark UIs (history server and live driver UIs) are reached through
	// the web proxy, whose Ingress host is a fixed prefix, not the release
	// or service name.
	"spark": func(namespace string) string { return fmt.Sprintf("spark-web-proxy-%s", namespace) },
}

// candidateHosts lists the Ingress hosts, in priority order, that could
// expose instance: its own release name first, then any role-specific
// convention that applies to it, then "<instance>[-console]-<namespace>" for an
// instance not named after its service, then the platform-packages catalog's own
// "<service>[-console]-<namespace>" convention, most service contexts
// (Trino, Superset, JupyterHub, Spark History, Polaris, ...) publish their
// endpoint at a host built from the service name and namespace rather than
// the release name, independently of whatever instance name the user chose.
func candidateHosts(instance *models.ServiceInstance, suffix string) []string {
	hosts := []string{fmt.Sprintf("%s.%s", instance.ReleaseName, suffix)}
	for _, role := range instance.Roles {
		if hostname, ok := roleHostConventions[role]; ok {
			hosts = append(hosts, fmt.Sprintf("%s.%s", hostname(instance.TargetNamespace), suffix))
		}
	}
	// The instance name the user chose, which packages publish as
	// "<instance>[-console]-<namespace>" so that two instances of a service in
	// one project get distinct hosts.
	if instance.Name != "" && instance.Name != instance.Service {
		hosts = append(hosts,
			fmt.Sprintf("%s-console-%s.%s", instance.Name, instance.TargetNamespace, suffix),
			fmt.Sprintf("%s-%s.%s", instance.Name, instance.TargetNamespace, suffix),
		)
	}
	if instance.Service != "" {
		hosts = append(hosts,
			fmt.Sprintf("%s-console-%s.%s", instance.Service, instance.TargetNamespace, suffix),
			fmt.Sprintf("%s-%s.%s", instance.Service, instance.TargetNamespace, suffix),
		)
	}
	return hosts
}

// namespaceIngressHosts maps each host served by an Ingress in a namespace to
// the HelmRelease that owns it (empty on error, so a lookup failure never fabricates a URL).
func (s *DefaultServiceService) namespaceIngressHosts(ctx context.Context, namespace string) map[string]string {
	ingressGVR := schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
	list, err := s.k8sClient.Resource(ingressGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return map[string]string{}
	}
	return ingressHostsFromItems(list.Items)
}

// Maps each Ingress host to its HelmRelease.
// Ingresses without a HelmRelease owner are ignored.
func ingressHostsFromItems(items []unstructured.Unstructured) map[string]string {
	hosts := map[string]string{}
	for i := range items {
		owner := items[i].GetLabels()["helm.toolkit.fluxcd.io/name"]
		if owner == "" {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(items[i].Object, "spec", "rules")
		for _, r := range rules {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			if host, ok := rm["host"].(string); ok && host != "" {
				hosts[host] = owner
			}
		}
	}
	return hosts
}

// enrichWithPodHealth overrides a "Ready" Release status when pods are actually unhealthy.
func (s *DefaultServiceService) enrichWithPodHealth(ctx context.Context, instances []models.ServiceInstance) {
	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}

	podsByNamespace := map[string][]unstructured.Unstructured{}
	for i := range instances {
		if instances[i].Status != "Ready" {
			continue
		}
		ns := instances[i].TargetNamespace
		if ns == "" {
			continue
		}
		if _, done := podsByNamespace[ns]; !done {
			podList, err := s.k8sClient.Resource(podGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				podsByNamespace[ns] = nil
			} else {
				podsByNamespace[ns] = podList.Items
			}
		}
		pods := podsByNamespace[ns]
		if pods == nil {
			continue
		}

		names, err := s.workloadNames(ctx, ns, instances[i].ReleaseName)
		if err != nil {
			continue
		}
		markPodHealth(&instances[i], s.checkPodHealth(pods, names, instances[i].Status))
	}
}

// enrichPodHealth is the single-instance counterpart of enrichWithPodHealth.
func (s *DefaultServiceService) enrichPodHealth(ctx context.Context, instance *models.ServiceInstance) {
	if instance == nil || instance.Status != "Ready" || instance.TargetNamespace == "" {
		return
	}
	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	podList, err := s.k8sClient.Resource(podGVR).Namespace(instance.TargetNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	names, err := s.workloadNames(ctx, instance.TargetNamespace, instance.ReleaseName)
	if err != nil {
		return
	}
	markPodHealth(instance, s.checkPodHealth(podList.Items, names, instance.Status))
}

// markPodHealth records the status the pods gave a Ready instance. A Ready
// phase whose pods fail becomes Degraded.
func markPodHealth(instance *models.ServiceInstance, status string) {
	if status == "Error" && instance.Status == "Ready" && instance.Phase == string(engine.PhaseReady) {
		instance.Phase = string(engine.PhaseDegraded)
	}
	instance.Status = status
}

func (s *DefaultServiceService) checkPodHealth(pods []unstructured.Unstructured, names []string, currentStatus string) string {
	for _, pod := range pods {
		if !matchesInstance(pod.GetName(), names) {
			continue
		}
		containerStatuses, _, _ := unstructured.NestedSlice(pod.Object, "status", "containerStatuses")
		for _, cs := range containerStatuses {
			csMap, ok := cs.(map[string]any)
			if !ok {
				continue
			}
			if name, ok := csMap["name"].(string); ok && s.isInfraSidecar(name) {
				continue
			}
			ready, _, _ := unstructured.NestedBool(csMap, "ready")
			if ready {
				continue
			}
			waiting, _, _ := unstructured.NestedMap(csMap, "state", "waiting")
			if reason, ok := waiting["reason"].(string); ok {
				if reason == "CrashLoopBackOff" || reason == "Error" || reason == "ImagePullBackOff" || reason == "ErrImagePull" {
					return "Error"
				}
			}
			terminated, _, _ := unstructured.NestedMap(csMap, "state", "terminated")
			if reason, ok := terminated["reason"].(string); ok && reason == "Error" {
				return "Error"
			}
		}
	}
	return currentStatus
}

func (s *DefaultServiceService) resolvePlatformService(ctx context.Context, name string) (*models.PlatformService, error) {
	services, err := s.engine.Catalog.Entries(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read platform services: %w", err)
	}

	for _, svc := range services {
		if svc.Name == name {
			return &svc, nil
		}
	}
	return nil, fmt.Errorf("service %q is not available in the platform", name)
}

// parameterSchemaURL names the parameter schema of a package while it is compiled.
const parameterSchemaURL = "mem://okdp/parameters.schema.json"

// maxParameterRecursionDepth bounds nested parameter traversal when scanning
// for embedded credentials.
const maxParameterRecursionDepth = 64

func checkParameterCredentials(params map[string]any) error {
	if badPath, found := findEmbeddedCredentials("", params, 0); found {
		return &schemaViolation{err: invalid("parameter %q must not contain embedded credentials", badPath)}
	}
	return nil
}

func findEmbeddedCredentials(path string, v any, depth int) (string, bool) {
	if depth > maxParameterRecursionDepth {
		return path, true
	}
	switch val := v.(type) {
	case string:
		if hasEmbeddedURLCredentials(val) {
			return path, true
		}
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			childPath := k
			if path != "" {
				childPath = path + "." + k
			}
			if bad, found := findEmbeddedCredentials(childPath, val[k], depth+1); found {
				return bad, true
			}
		}
	case map[string]string:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			childPath := k
			if path != "" {
				childPath = path + "." + k
			}
			if hasEmbeddedURLCredentials(val[k]) {
				return childPath, true
			}
		}
	case []any:
		for i, elem := range val {
			childPath := fmt.Sprintf("%s[%d]", path, i)
			if bad, found := findEmbeddedCredentials(childPath, elem, depth+1); found {
				return bad, true
			}
		}
	case []string:
		for i, elem := range val {
			if hasEmbeddedURLCredentials(elem) {
				return fmt.Sprintf("%s[%d]", path, i), true
			}
		}
	}
	return "", false
}

func (s *DefaultServiceService) validateParameters(ctx context.Context, serviceName, tag string, params map[string]any) error {
	if err := checkParameterCredentials(params); err != nil {
		return err
	}

	schemaMap, err := s.engine.Catalog.ParameterSchema(ctx, serviceName, tag)
	if err != nil {
		return fmt.Errorf("could not fetch schema for %s@%s: %w", serviceName, tag, err)
	}
	if len(schemaMap) == 0 {
		return nil
	}

	schemaJSON, err := json.Marshal(schemaMap)
	if err != nil {
		logrus.WithError(err).Warn("Failed to marshal parameter schema JSON")
		return fmt.Errorf("invalid parameter schema for %s@%s: failed to marshal schema: %w", serviceName, tag, err)
	}

	compiler := jsonschema.NewCompiler()
	// No loader: a $ref outside the schema is never read, from disk or elsewhere.
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		logrus.WithError(err).Warn("Failed to unmarshal parameter schema JSON")
		return fmt.Errorf("invalid parameter schema for %s@%s: failed to unmarshal schema: %w", serviceName, tag, err)
	}
	// An absolute URL: a relative one is resolved against the server's working
	// directory, which then shows in every validation message.
	if err := compiler.AddResource(parameterSchemaURL, schemaDoc); err != nil {
		logrus.WithError(err).Warn("Failed to add parameter schema resource")
		return fmt.Errorf("invalid parameter schema for %s@%s: failed to load schema resource: %w", serviceName, tag, err)
	}

	sch, err := compiler.Compile(parameterSchemaURL)
	if err != nil {
		logrus.WithError(err).Warn("Failed to compile parameter schema")
		return fmt.Errorf("invalid parameter schema for %s@%s: failed to compile schema: %w", serviceName, tag, err)
	}

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("failed to marshal parameters: %w", err)
	}

	var paramsAny any
	if err := json.Unmarshal(paramsJSON, &paramsAny); err != nil {
		return fmt.Errorf("failed to unmarshal parameters: %w", err)
	}

	if err := sch.Validate(paramsAny); err != nil {
		return &schemaViolation{err: err}
	}
	return nil
}

// --- Pod operations ---

func (s *DefaultServiceService) isInfraSidecar(containerName string) bool {
	for _, prefix := range s.sidecarPrefixes {
		if strings.HasPrefix(containerName, prefix) {
			return true
		}
	}
	return false
}

// workloadNames lists the app.kubernetes.io/instance values of what the
// instance stored as releaseName runs.
func (s *DefaultServiceService) workloadNames(ctx context.Context, namespace, releaseName string) ([]string, error) {
	workloads, err := s.engine.Observer.Workloads(ctx, namespace, releaseName)
	if err != nil {
		return nil, err
	}
	return workloads.Names, nil
}

func (s *DefaultServiceService) releaseInstanceSelector(ctx context.Context, project, releaseName string) (string, error) {
	names, err := s.workloadNames(ctx, project, releaseName)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("app.kubernetes.io/instance in (%s)", strings.Join(names, ",")), nil
}

func matchesInstance(name string, instanceNames []string) bool {
	for _, n := range instanceNames {
		if name == n || strings.HasPrefix(name, n+"-") {
			return true
		}
	}
	return false
}

func (s *DefaultServiceService) ListPods(ctx context.Context, project, serviceName string) ([]models.Pod, error) {
	releaseName := s.engine.Observer.EngineName(engine.InstanceRef{Project: project, Name: serviceName})

	selector, err := s.releaseInstanceSelector(ctx, project, releaseName)
	if err != nil {
		return nil, err
	}

	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	podList, err := s.k8sClient.Resource(podGVR).Namespace(project).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	var result []models.Pod
	for _, item := range podList.Items {
		result = append(result, s.unstructuredToPod(&item))
	}
	return result, nil
}

func (s *DefaultServiceService) GetPodLogs(ctx context.Context, project, podName, container string, tailLines int64, follow bool) (io.ReadCloser, error) {
	opts := &corev1.PodLogOptions{
		Follow: follow,
	}
	if tailLines > 0 {
		opts.TailLines = &tailLines
	}
	if container != "" {
		opts.Container = container
	}

	stream, err := s.typedClient.CoreV1().Pods(project).GetLogs(podName, opts).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod logs: %w", err)
	}
	return stream, nil
}

func (s *DefaultServiceService) unstructuredToPod(u *unstructured.Unstructured) models.Pod {
	pod := models.Pod{
		Name: u.GetName(),
	}

	// Phase
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	pod.Status = phase

	// Age
	ts := u.GetCreationTimestamp()
	if !ts.IsZero() {
		d := time.Since(ts.Time)
		switch {
		case d.Hours() >= 24:
			pod.Age = fmt.Sprintf("%dd", int(d.Hours()/24))
		case d.Hours() >= 1:
			pod.Age = fmt.Sprintf("%dh", int(d.Hours()))
		default:
			pod.Age = fmt.Sprintf("%dm", int(d.Minutes()))
		}
	}

	// Containers (filter sidecars)
	containerStatuses, _, _ := unstructured.NestedSlice(u.Object, "status", "containerStatuses")
	readyCount := 0
	totalCount := 0
	var restarts int32

	for _, cs := range containerStatuses {
		csMap, ok := cs.(map[string]any)
		if !ok {
			continue
		}
		name, _ := csMap["name"].(string)
		if s.isInfraSidecar(name) {
			continue
		}
		totalCount++
		image, _ := csMap["image"].(string)
		ready, _ := csMap["ready"].(bool)
		if ready {
			readyCount++
		}
		rc, _ := csMap["restartCount"].(int64)
		restarts += int32(rc)

		pod.Containers = append(pod.Containers, models.Container{
			Name:  name,
			Image: image,
			Ready: ready,
		})
	}

	pod.Ready = fmt.Sprintf("%d/%d", readyCount, totalCount)
	pod.Restarts = restarts

	return pod
}

// baseInstance describes an observed instance in the API model.
func baseInstance(v engine.InstanceView) *models.ServiceInstance {
	createdAt := ""
	if !v.CreatedAt.IsZero() {
		createdAt = v.CreatedAt.Format(time.RFC3339)
	}

	phase := observedPhase(v.Status)
	statusMessage := ""
	if phase != engine.PhaseReady {
		statusMessage = truncateMessage(v.Status.Message)
	}

	return &models.ServiceInstance{
		Name:              v.Ref.Name,
		ReleaseName:       v.EngineName,
		Service:           v.Service,
		ServiceTag:        v.Version,
		Status:            LegacyStatus(v.Status.Phase),
		StatusMessage:     statusMessage,
		TargetNamespace:   v.TargetNamespace,
		Roles:             v.Roles,
		Parameters:        v.Parameters,
		Connections:       boundConnections(v.Bindings),
		CreatedAt:         createdAt,
		Phase:             string(phase),
		DesiredRevision:   v.Status.DesiredRevision,
		PublishedRevision: v.Status.PublishedRevision,
		AppliedRevision:   v.Status.AppliedRevision,
	}
}

// observedPhase is the phase an instance shows: Deleting once its deletion is
// requested, Unknown when the engine reported none.
func observedPhase(status engine.InstanceStatus) engine.Phase {
	switch {
	case status.Deleting:
		return engine.PhaseDeleting
	case status.Phase == "":
		return engine.PhaseUnknown
	default:
		return status.Phase
	}
}

// LegacyStatus is the value of ServiceInstance.Status for a phase: Ready,
// Error and Updating keep their name, every other phase reads Installing.
func LegacyStatus(phase engine.Phase) string {
	switch phase {
	case engine.PhaseReady:
		return "Ready"
	case engine.PhaseError, engine.PhaseDegraded:
		return "Error"
	case engine.PhaseUpdating:
		return "Updating"
	default:
		return "Installing"
	}
}

// boundConnections describes what an instance is wired to, from the bindings
// the engine reported: the resolved ones first, then every watched candidate.
//
// The two lists do not mean what their names suggest. A connectionRef with no
// kind resolves by looking on both sides, so watchedInputConnections holds
// every *candidate*, a Connection and a ClusterConnection per name, not a set
// of pending bindings. Listing them as they come showed "demo-db
// (ClusterConnection) waiting" next to the Connection that had resolved, for a
// ClusterConnection that never existed. Candidates are therefore grouped by
// name: a name that resolved shows once, resolved. A name that resolved nowhere
// shows once, waiting, which is the case a reader opens this page for.
func boundConnections(bindings []engine.ConnectionBinding) []models.ServiceConnection {
	byName := map[string]models.ServiceConnection{}
	order := make([]string, 0, len(bindings))

	remember := func(ref engine.ConnectionBinding, resolved bool) {
		current, seen := byName[ref.Name]
		if !seen {
			order = append(order, ref.Name)
		}
		// The state is the furthest any candidate of the name reached.
		state := ref.State
		if seen && engine.ConnectionState(current.State).Rank() > state.Rank() {
			state = engine.ConnectionState(current.State)
		}
		// A resolved candidate always wins over a pending one.
		if seen && current.Resolved {
			current.State = string(state)
			byName[ref.Name] = current
			return
		}
		byName[ref.Name] = models.ServiceConnection{
			Name: ref.Name, Namespace: ref.Namespace, Kind: ref.Kind, Resolved: resolved, State: string(state),
		}
	}

	for _, binding := range bindings {
		remember(binding, binding.Bound)
	}

	if len(order) == 0 {
		return nil
	}
	sort.Strings(order)
	out := make([]models.ServiceConnection, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}

// GetServiceMetrics aggregates live CPU/memory usage from the metrics-server
// for every pod belonging to a service instance, against the total limits
// read from the pods' container specs.
func (s *DefaultServiceService) GetProjectMetrics(ctx context.Context, project string) (map[string]*models.ServiceMetrics, error) {
	instances, err := s.ListServices(ctx, project)
	if err != nil {
		return nil, err
	}

	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	allPods, err := s.k8sClient.Resource(podGVR).Namespace(project).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	metricsByPod := s.listPodMetricsByName(ctx, project)

	metrics := make(map[string]*models.ServiceMetrics, len(instances))
	for _, inst := range instances {
		names, err := s.workloadNames(ctx, project, inst.ReleaseName)
		if err != nil {
			continue
		}
		var pods []unstructured.Unstructured
		for _, pod := range allPods.Items {
			if slices.Contains(names, pod.GetLabels()["app.kubernetes.io/instance"]) {
				pods = append(pods, pod)
			}
		}
		metrics[inst.Name] = buildServiceMetrics(pods, metricsByPod)
	}
	return metrics, nil
}

func (s *DefaultServiceService) listPodMetricsByName(ctx context.Context, namespace string) map[string]*unstructured.Unstructured {
	metricsGVR := schema.GroupVersionResource{
		Group:    "metrics.k8s.io",
		Version:  "v1beta1",
		Resource: "pods",
	}
	byName := map[string]*unstructured.Unstructured{}
	list, err := s.k8sClient.Resource(metricsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		logrus.Debugf("metrics for namespace %s unavailable: %v", namespace, err)
		return byName
	}
	for i := range list.Items {
		byName[list.Items[i].GetName()] = &list.Items[i]
	}
	return byName
}

func buildServiceMetrics(pods []unstructured.Unstructured, metricsByPod map[string]*unstructured.Unstructured) *models.ServiceMetrics {
	var cpuLimit, memLimit float64
	var cpuUsed, memUsed float64
	cpuUsedAvailable := false
	memUsedAvailable := false

	for _, pod := range pods {
		containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
		for _, c := range containers {
			container, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			resources, _ := container["resources"].(map[string]interface{})
			if resources == nil {
				continue
			}
			for _, bucket := range []string{"limits", "requests"} {
				quantities, _ := resources[bucket].(map[string]interface{})
				if quantities == nil {
					continue
				}
				if v, ok := quantities["cpu"].(string); ok && v != "" {
					if cores, err := parseCPUQuantity(v); err != nil {
						logrus.WithError(err).WithField("pod", pod.GetName()).Warn("skipping unparseable CPU limit")
					} else {
						cpuLimit += cores
					}
				}
				if v, ok := quantities["memory"].(string); ok && v != "" {
					if bytes, err := parseMemoryQuantity(v); err != nil {
						logrus.WithError(err).WithField("pod", pod.GetName()).Warn("skipping unparseable memory limit")
					} else {
						memLimit += bytes
					}
				}
				break
			}
		}
	}

	for _, pod := range pods {
		podMetrics, ok := metricsByPod[pod.GetName()]
		if !ok {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(podMetrics.Object, "containers")
		for _, c := range containers {
			container, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			usage, _ := container["usage"].(map[string]interface{})
			if usage == nil {
				continue
			}
			if v, ok := usage["cpu"].(string); ok && v != "" {
				if cores, err := parseCPUQuantity(v); err != nil {
					logrus.WithError(err).WithField("pod", pod.GetName()).Warn("skipping unparseable CPU usage from metrics-server")
				} else {
					cpuUsed += cores
					cpuUsedAvailable = true
				}
			}
			if v, ok := usage["memory"].(string); ok && v != "" {
				if bytes, err := parseMemoryQuantity(v); err != nil {
					logrus.WithError(err).WithField("pod", pod.GetName()).Warn("skipping unparseable memory usage from metrics-server")
				} else {
					memUsed += bytes
					memUsedAvailable = true
				}
			}
		}
	}

	metrics := &models.ServiceMetrics{
		CPU: models.MetricValue{
			UsedRaw:   cpuUsed,
			LimitRaw:  cpuLimit,
			Used:      formatCPU(cpuUsed),
			Limit:     formatCPU(cpuLimit),
			Pct:       ratio(cpuUsed, cpuLimit),
			Available: cpuUsedAvailable,
		},
		Memory: models.MetricValue{
			UsedRaw:   memUsed,
			LimitRaw:  memLimit,
			Used:      formatMemory(memUsed),
			Limit:     formatMemory(memLimit),
			Pct:       ratio(memUsed, memLimit),
			Available: memUsedAvailable,
		},
	}
	return metrics
}

func (s *DefaultServiceService) GetServiceMetrics(ctx context.Context, project, serviceName string) (*models.ServiceMetrics, error) {
	releaseName := s.engine.Observer.EngineName(engine.InstanceRef{Project: project, Name: serviceName})

	selector, err := s.releaseInstanceSelector(ctx, project, releaseName)
	if err != nil {
		return nil, err
	}

	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	podList, err := s.k8sClient.Resource(podGVR).Namespace(project).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	metricsByPod := s.listPodMetricsByName(ctx, project)
	return buildServiceMetrics(podList.Items, metricsByPod), nil
}

// parseCPUQuantity parses a Kubernetes CPU quantity string (e.g. "500m",
// "2", "1500000000n", "1.5") and returns the value in whole cores. Wraps
// k8s.io/apimachinery resource.ParseQuantity, the canonical parser used
// throughout the Kubernetes ecosystem, so we inherit correct handling of
// every SI suffix (n, u, m, k, M, G, T, P, E) and binary suffix (Ki, Mi,
// Gi, Ti, Pi, Ei) as well as decimal and scientific notation.
// Returns an error when s is not a valid quantity; callers are expected
// to log a warning and skip the faulty container, not fail the whole
// request.
func parseCPUQuantity(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, fmt.Errorf("invalid CPU quantity %q: %w", s, err)
	}
	// AsApproximateFloat64 returns the value in the quantity's base units.
	// For CPU, the base unit is already "cores" (e.g. "500m" → 0.5).
	return q.AsApproximateFloat64(), nil
}

// parseMemoryQuantity parses a Kubernetes memory quantity string (e.g.
// "512Mi", "2Gi", "1024", "1.5Gi") and returns the value in bytes. Same
// rationale as parseCPUQuantity, we delegate to resource.ParseQuantity.
func parseMemoryQuantity(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, fmt.Errorf("invalid memory quantity %q: %w", s, err)
	}
	// For memory, the base unit is bytes.
	return q.AsApproximateFloat64(), nil
}

// formatCPU returns a compact human-readable string in cores.
func formatCPU(cores float64) string {
	if cores == 0 {
		return "0"
	}
	if cores < 1 {
		return fmt.Sprintf("%.3f", cores)
	}
	return fmt.Sprintf("%.2f", cores)
}

// formatMemory picks the right binary unit for a byte value.
func formatMemory(bytes float64) string {
	if bytes == 0 {
		return "0"
	}
	units := []struct {
		threshold float64
		suffix    string
	}{
		{1024 * 1024 * 1024, "Gi"},
		{1024 * 1024, "Mi"},
		{1024, "Ki"},
	}
	for _, u := range units {
		if bytes >= u.threshold {
			return fmt.Sprintf("%.2f%s", bytes/u.threshold, u.suffix)
		}
	}
	return fmt.Sprintf("%.0fB", bytes)
}

func ratio(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	r := a / b
	if r > 1 {
		return 1
	}
	return r
}
