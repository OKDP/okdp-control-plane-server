package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Catalog management errors. Handlers map these to HTTP 400/404/409.
var (
	ErrCatalogValidation      = errors.New("invalid catalog service")
	ErrCatalogServiceExists   = errors.New("service already exists in the catalog")
	ErrCatalogServiceNotFound = errors.New("service not found in the catalog")
)

// serviceNameRe matches a DNS-style identifier (lowercase alphanumerics and dashes).
var serviceNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// servicesResource names the resource in the not-found and conflict errors
// the handlers map to 404 and 409.
var servicesResource = schema.GroupResource{Group: "okdp.io", Resource: "services"}

// ServiceService manages platform services (deploy, monitor, delete) and exposes the catalog.
//
// Desired state is written to the deployments Git repository; observed state
// is read from the cluster (instance descriptors, workloads, and the objects
// of the GitOps engine).
type ServiceService interface {
	GetPlatformServices(ctx context.Context) ([]models.PlatformService, error)
	// AddPlatformService exposes a new service in the catalog (platform/catalog.yaml).
	AddPlatformService(ctx context.Context, svc models.PlatformService) (*models.PlatformService, error)
	// UpdatePlatformService updates an existing catalog service (name from the path).
	UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) (*models.PlatformService, error)
	// RemovePlatformService removes a service from the catalog.
	RemovePlatformService(ctx context.Context, name string) error
	DeployService(ctx context.Context, project string, req models.ServiceRequest) (*models.ServiceInstance, error)
	ListServices(ctx context.Context, project string) ([]models.ServiceInstance, error)
	GetService(ctx context.Context, project, name string) (*models.ServiceInstance, error)
	UpdateServiceParameters(ctx context.Context, project, name string, req models.ServiceUpdateRequest) (*models.ServiceInstance, error)
	DeleteService(ctx context.Context, project, name string) error
	// WatchServices streams the instances of a project as they change, until
	// ctx ends or an underlying watch closes (the channel is then closed).
	WatchServices(ctx context.Context, project string) (<-chan ServiceEvent, error)

	GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error)

	GetIngressSuffix(ctx context.Context) (string, error)

	GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error)

	ListPods(ctx context.Context, project, serviceName string) ([]models.Pod, error)
	// GetPodLogs streams the logs of a pod of the instance serviceName; a pod
	// of another instance, or of none, is not found.
	GetPodLogs(ctx context.Context, project, serviceName, podName, container string, tailLines int64, follow bool) (io.ReadCloser, error)
	GetServiceMetrics(ctx context.Context, project, serviceName string) (*models.ServiceMetrics, error)
	GetProjectMetrics(ctx context.Context, project string) (map[string]*models.ServiceMetrics, error)
}

// ServiceEvent is one change of a project's instances, as streamed to the
// console: Type is ADDED, MODIFIED or DELETED.
type ServiceEvent struct {
	Type   string                 `json:"type"`
	Object models.ServiceInstance `json:"object"`
}

// ServiceDeps gathers what DefaultServiceService needs.
type ServiceDeps struct {
	Deployments     *gitops.Deployments
	PlatformRepo    repository.PlatformRepository
	CatalogWriter   repository.CatalogWriterRepository
	SchemaService   PackageSchemaService
	OidcProvisioner provisioning.OidcClientProvisioner
	Engine          repository.EngineAdapter
	Descriptors     repository.DescriptorRepository
	K8sClient       dynamic.Interface
	TypedClient     kubernetes.Interface
	SidecarPrefixes []string
	// Changes is shared with the other writers of the deployments repository,
	// so a live stream hears about their commits too. Created when nil.
	Changes *ChangeNotifier
}

type DefaultServiceService struct {
	deployments     *gitops.Deployments
	platformRepo    repository.PlatformRepository
	catalogWriter   repository.CatalogWriterRepository
	schemaService   PackageSchemaService
	oidcProvisioner provisioning.OidcClientProvisioner
	engine          repository.EngineAdapter
	descriptors     repository.DescriptorRepository
	k8sClient       dynamic.Interface
	typedClient     kubernetes.Interface
	sidecarPrefixes []string
	changes         *ChangeNotifier
}

func NewDefaultServiceService(d ServiceDeps) *DefaultServiceService {
	if d.Changes == nil {
		d.Changes = NewChangeNotifier()
	}
	return &DefaultServiceService{
		deployments:     d.Deployments,
		platformRepo:    d.PlatformRepo,
		catalogWriter:   d.CatalogWriter,
		schemaService:   d.SchemaService,
		oidcProvisioner: d.OidcProvisioner,
		engine:          d.Engine,
		descriptors:     d.Descriptors,
		k8sClient:       d.K8sClient,
		typedClient:     d.TypedClient,
		sidecarPrefixes: d.SidecarPrefixes,
		changes:         d.Changes,
	}
}

// --- Platform services ---

func (s *DefaultServiceService) GetPlatformServices(ctx context.Context) ([]models.PlatformService, error) {
	services, err := s.platformRepo.GetPlatformServices(ctx)
	if err != nil {
		return nil, err
	}

	versions := s.schemaService.ListVersionsForServices(ctx, services)
	for i, svc := range services {
		if tags, ok := versions[svc.Name]; ok {
			services[i].Versions = tags
		}
	}

	return services, nil
}

// AddPlatformService validates and appends a new service to the catalog.
func (s *DefaultServiceService) AddPlatformService(ctx context.Context, svc models.PlatformService) (*models.PlatformService, error) {
	if s.catalogWriter == nil {
		return nil, fmt.Errorf("catalog management is not available")
	}
	if err := normalizeAndValidateCatalogService(&svc); err != nil {
		return nil, err
	}

	existing, err := s.platformRepo.GetPlatformServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read catalog: %w", err)
	}
	for _, e := range existing {
		if e.Name == svc.Name {
			return nil, fmt.Errorf("%w: %q", ErrCatalogServiceExists, svc.Name)
		}
	}

	if err := s.validateVersionsInRegistry(ctx, svc); err != nil {
		return nil, err
	}

	if err := s.catalogWriter.AddPlatformService(ctx, svc); err != nil {
		return nil, err
	}
	s.platformRepo.Invalidate()
	return &svc, nil
}

// UpdatePlatformService validates and replaces an existing catalog service.
func (s *DefaultServiceService) UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) (*models.PlatformService, error) {
	if s.catalogWriter == nil {
		return nil, fmt.Errorf("catalog management is not available")
	}
	svc.Name = name // the path is the source of truth for identity
	if err := normalizeAndValidateCatalogService(&svc); err != nil {
		return nil, err
	}

	if _, err := s.findCatalogService(ctx, name); err != nil {
		return nil, err
	}

	if err := s.validateVersionsInRegistry(ctx, svc); err != nil {
		return nil, err
	}

	if err := s.catalogWriter.UpdatePlatformService(ctx, name, svc); err != nil {
		return nil, err
	}
	s.platformRepo.Invalidate()
	return &svc, nil
}

// RemovePlatformService removes a service from the catalog.
func (s *DefaultServiceService) RemovePlatformService(ctx context.Context, name string) error {
	if s.catalogWriter == nil {
		return fmt.Errorf("catalog management is not available")
	}
	if _, err := s.findCatalogService(ctx, name); err != nil {
		return err
	}
	if err := s.catalogWriter.RemovePlatformService(ctx, name); err != nil {
		return err
	}
	s.platformRepo.Invalidate()
	return nil
}

// findCatalogService returns the catalog service by name or ErrCatalogServiceNotFound.
func (s *DefaultServiceService) findCatalogService(ctx context.Context, name string) (*models.PlatformService, error) {
	existing, err := s.platformRepo.GetPlatformServices(ctx)
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

// validateVersionsInRegistry rejects versions that don't exist in the OCI registry
// for the service's package. Strict: if the registry can't be queried (package not
// published / unreachable), the service is rejected. Reuses the existing OCI tag
// listing used by the read path.
func (s *DefaultServiceService) validateVersionsInRegistry(ctx context.Context, svc models.PlatformService) error {
	if s.schemaService == nil {
		return nil
	}
	tags, err := s.schemaService.ListPackageTags(ctx, svc.Name, svc.Repository)
	if err != nil {
		return fmt.Errorf("%w: could not verify package %q in the registry (%v)", ErrCatalogValidation, svc.Name, err)
	}
	available := make(map[string]bool, len(tags))
	for _, t := range tags {
		available[t] = true
	}
	var missing []string
	for _, v := range svc.Versions {
		if !available[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: version(s) %v not found in the registry for %q (available: %v)", ErrCatalogValidation, missing, svc.Name, tags)
	}
	return nil
}

// normalizeAndValidateCatalogService enforces the catalog rules and fills the
// default version when omitted. Returns an error wrapping ErrCatalogValidation.
func normalizeAndValidateCatalogService(svc *models.PlatformService) error {
	svc.Name = strings.TrimSpace(svc.Name)
	if svc.Name == "" {
		return fmt.Errorf("%w: name is required", ErrCatalogValidation)
	}
	if !serviceNameRe.MatchString(svc.Name) {
		return fmt.Errorf("%w: name %q must be a lowercase DNS-style identifier", ErrCatalogValidation, svc.Name)
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

// --- Instances: desired state in Git ---

// ReleaseNameTakenError is a new instance whose release name <p>-<i> another
// project's instance (or a platform component) already uses. The handler
// answers 409 with its own code, distinct from a duplicate instance.
type ReleaseNameTakenError struct{ *gitops.ErrReleaseTaken }

// IsReleaseNameTaken reports whether err is a release-name collision.
func IsReleaseNameTaken(err error) bool {
	var taken *ReleaseNameTakenError
	return errors.As(err, &taken)
}

// gitError maps the errors of the deployments repository to the Kubernetes
// API errors the handlers already turn into 404 and 409.
func gitError(err error, name string) error {
	var taken *gitops.ErrReleaseTaken
	switch {
	case err == nil:
		return nil
	case errors.As(err, &taken):
		return &ReleaseNameTakenError{taken}
	case errors.Is(err, gitops.ErrNotFound):
		return apierrors.NewNotFound(servicesResource, name)
	case errors.Is(err, gitops.ErrExists):
		return apierrors.NewAlreadyExists(servicesResource, name)
	default:
		return err
	}
}

// chartURL is the OCI chart reference of a catalog service, without tag.
func chartURL(packageRepo, service string) string {
	return "oci://" + strings.TrimSuffix(strings.TrimPrefix(packageRepo, "oci://"), "/") + "/" + service
}

func (s *DefaultServiceService) DeployService(ctx context.Context, project string, req models.ServiceRequest) (*models.ServiceInstance, error) {
	if req.Service == "" {
		return nil, invalid("service name is required")
	}

	svc, err := s.resolvePlatformService(ctx, req.Service)
	if err != nil {
		return nil, err
	}

	deployTag := req.Tag
	if deployTag == "" {
		deployTag = svc.DefaultVersion
	}

	instanceName := req.InstanceName
	if instanceName == "" {
		instanceName = req.Service
	}
	if err := gitops.ValidateName("instance", instanceName); err != nil {
		return nil, invalid("%v", err)
	}

	if err := s.validateParameters(ctx, req.Service, deployTag, req.Parameters); err != nil {
		return nil, err
	}

	packageRepo, err := s.platformRepo.GetPackageRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the chart repository: %w", err)
	}
	if svc.Repository != "" {
		packageRepo = svc.Repository
	}

	connections, err := s.deployments.ListConnections(ctx, project)
	if err != nil {
		return nil, err
	}

	values := req.Parameters
	if values == nil {
		values = map[string]any{}
	}
	state := gitops.InstanceState{
		Instance: gitops.Instance{
			Name:        instanceName,
			Project:     project,
			Service:     req.Service,
			Chart:       chartURL(packageRepo, req.Service),
			Version:     deployTag,
			Connections: referencedConnections(values, connectionNames(connections)),
		},
		Values: values,
	}
	if err := state.Instance.Validate(); err != nil {
		return nil, invalid("%v", err)
	}

	revision, err := s.deployments.CreateInstance(ctx, auth.ActorFrom(ctx), state)
	if err != nil {
		return nil, gitError(err, instanceName)
	}
	s.changes.Notify(project, instanceName, "ADDED")

	instance := s.assemble(state, repository.EngineStatus{}, nil, nil, nil)
	instance.Revision = revision
	return &instance, nil
}

// errStaleValidation reports that the declaration changed between the
// validation of a patch and its write.
var errStaleValidation = errors.New("the instance changed while its parameters were validated")

// maxValidationReplays bounds how many times a patch is validated again
// because the declaration kept changing under it.
const maxValidationReplays = 3

// UpdateServiceParameters applies a JSON Merge Patch to the parameters of an
// instance (and its chart version when req.Tag is set).
//
// Validation reads the catalog and the chart schema, through the deployments
// repository: it must not run inside UpdateInstance, whose callback holds the
// store's write lock (a read there waits for itself). The patch is validated
// against the current declaration first; the write then re-applies it to the
// latest declaration and only proceeds when that declaration is the one that
// was validated, otherwise the whole validation runs again.
func (s *DefaultServiceService) UpdateServiceParameters(ctx context.Context, project, name string, req models.ServiceUpdateRequest) (*models.ServiceInstance, error) {
	connections, err := s.deployments.ListConnections(ctx, project)
	if err != nil {
		return nil, err
	}
	known := connectionNames(connections)

	for attempt := 0; ; attempt++ {
		current, err := s.deployments.GetInstance(ctx, project, name)
		if err != nil {
			return nil, gitError(err, name)
		}
		version := current.Instance.Version
		if req.Tag != "" {
			version = req.Tag
		}
		// JSON Merge Patch (RFC 7386): a submitted key replaces the stored
		// one, null deletes it, objects merge recursively, arrays replace.
		values := MergePatch(current.Values, req.Parameters)
		if err := s.validateParameters(ctx, current.Instance.Service, version, values); err != nil {
			return nil, err
		}

		state, revision, err := s.deployments.UpdateInstance(ctx, auth.ActorFrom(ctx), project, name, func(st *gitops.InstanceState) error {
			if st.Instance.Service != current.Instance.Service || st.Instance.Version != current.Instance.Version ||
				!reflect.DeepEqual(st.Values, current.Values) {
				return errStaleValidation
			}
			st.Instance.Version = version
			st.Values = MergePatch(st.Values, req.Parameters)
			st.Instance.Connections = referencedConnections(st.Values, known)
			return nil
		})
		if errors.Is(err, errStaleValidation) {
			if attempt < maxValidationReplays {
				continue
			}
			return nil, fmt.Errorf("%w: %v", gitops.ErrConflict, err)
		}
		if err != nil {
			return nil, gitError(err, name)
		}
		s.changes.Notify(project, name, "MODIFIED")

		instance, err := s.GetService(ctx, project, name)
		if err != nil {
			fallback := s.assemble(*state, repository.EngineStatus{}, nil, nil, nil)
			instance = &fallback
		}
		instance.Revision = revision
		return instance, nil
	}
}

func (s *DefaultServiceService) DeleteService(ctx context.Context, project, name string) error {
	if _, err := s.deployments.DeleteInstance(ctx, auth.ActorFrom(ctx), project, name); err != nil {
		return gitError(err, name)
	}
	s.changes.Notify(project, name, "DELETED")

	releaseName := gitops.ReleaseName(project, name)
	s.cleanupOidcClient(ctx, releaseName)
	s.cleanupUserResources(ctx, project, releaseName)
	return nil
}

// cleanupOidcClient best-effort unregisters the OIDC client of a deleted
// service through the configured provisioning backend (no-op when
// identity.provisioning.provider is unset/none).
func (s *DefaultServiceService) cleanupOidcClient(ctx context.Context, releaseName string) {
	if s.oidcProvisioner == nil {
		return
	}
	if err := s.oidcProvisioner.DeleteClient(ctx, releaseName); err != nil {
		// Warn, not Debug: a client left registered outlives the service that
		// owned it and nothing else reports it.
		logrus.WithError(err).WithField("oidcClient", releaseName).Warn("OidcClient cleanup failed")
	} else {
		logrus.WithField("oidcClient", releaseName).Info("Cleaned up OidcClient")
	}
}

// MergePatch applies patch to target as a JSON Merge Patch (RFC 7386) and
// returns the result: a null value removes the key, a nested object merges
// into the stored one (recursively), anything else (arrays included)
// replaces it. target is not modified. The result is never nil.
func MergePatch(target, patch map[string]any) map[string]any {
	out := make(map[string]any, len(target)+len(patch))
	for k, v := range target {
		out[k] = v
	}
	for k, v := range patch {
		if v == nil {
			delete(out, k)
			continue
		}
		if patchObject, ok := v.(map[string]any); ok {
			current, _ := out[k].(map[string]any)
			out[k] = MergePatch(current, patchObject)
			continue
		}
		out[k] = v
	}
	return out
}

// connectionNames lists the external connections of a project.
func connectionNames(connections []gitops.Connection) map[string]bool {
	names := make(map[string]bool, len(connections))
	for _, c := range connections {
		names[c.Name] = true
	}
	return names
}

// referencedConnections lists, sorted, the external connections the values
// name: the connection files to layer in so the chart finds
// connections.<name>. A connection reference is a plain string parameter
// (x-okdp-connection-ref), possibly nested (a catalog of a Trino), so every
// string of the values is a candidate; only declared connections match.
// Layering one in that no parameter actually uses is harmless: the chart
// ignores connections it is not asked for.
func referencedConnections(values map[string]any, known map[string]bool) []string {
	found := map[string]bool{}
	walkStrings(values, func(v string) {
		if known[v] {
			found[v] = true
		}
	})
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// walkStrings calls fn on every string of a JSON-like value.
func walkStrings(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case map[string]any:
		for _, item := range t {
			walkStrings(item, fn)
		}
	case []any:
		for _, item := range t {
			walkStrings(item, fn)
		}
	}
}

// --- Instances: observed state from the cluster ---

// projectObservation is what the cluster says about a project's releases.
type projectObservation struct {
	engine      map[string]repository.EngineStatus
	descriptors map[string]*repository.Descriptor
	// outputs are the connections the deployed instances provide, by name.
	outputs map[string]bool
	pods    []unstructured.Unstructured
}

func (s *DefaultServiceService) observe(ctx context.Context, project string) projectObservation {
	obs := projectObservation{
		engine:      map[string]repository.EngineStatus{},
		descriptors: map[string]*repository.Descriptor{},
		outputs:     map[string]bool{},
	}
	if s.engine != nil {
		if statuses, err := s.engine.List(ctx, project); err != nil {
			logrus.WithError(err).WithField("engine", s.engine.Name()).Warn("Could not read the GitOps engine status")
		} else {
			obs.engine = statuses
		}
	}
	if s.descriptors != nil {
		if list, err := s.descriptors.List(ctx, project); err != nil {
			logrus.WithError(err).Warn("Could not read the instance descriptors")
		} else {
			for i := range list {
				obs.descriptors[list[i].Release] = &list[i]
				for _, o := range list[i].Outputs {
					obs.outputs[o.Name] = true
				}
			}
		}
	}
	if s.k8sClient != nil {
		podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
		if list, err := s.k8sClient.Resource(podGVR).Namespace(project).List(ctx, metav1.ListOptions{LabelSelector: repository.LabelAppInstance}); err == nil {
			obs.pods = list.Items
		}
	}
	return obs
}

// assemble builds the console view of an instance from its declaration and
// what the cluster reports.
func (s *DefaultServiceService) assemble(st gitops.InstanceState, engine repository.EngineStatus, descriptor *repository.Descriptor, externals map[string]bool, obs *projectObservation) models.ServiceInstance {
	inst := st.Instance
	release := inst.ReleaseName()
	instance := models.ServiceInstance{
		Name:            inst.Name,
		ReleaseName:     release,
		Service:         inst.Service,
		ServiceTag:      inst.Version,
		TargetNamespace: inst.Project,
		Parameters:      st.Values,
	}

	instance.Status, instance.StatusMessage = instanceStatus(inst, engine, s.engineName())
	if !engine.CreatedAt.IsZero() {
		instance.CreatedAt = engine.CreatedAt.UTC().Format(time.RFC3339)
	}
	if descriptor != nil {
		instance.URL = descriptor.URL
		instance.Usage = descriptor.Usage
		if instance.CreatedAt == "" && !descriptor.CreatedAt.IsZero() {
			instance.CreatedAt = descriptor.CreatedAt.UTC().Format(time.RFC3339)
		}
	}
	if obs != nil && instance.Status == repository.PhaseReady {
		instance.Status = s.checkPodHealth(obs.pods, release, instance.Status)
	}
	instance.Connections = boundConnections(st, externals, obs)
	return instance
}

func (s *DefaultServiceService) engineName() string {
	if s.engine == nil {
		return "the GitOps engine"
	}
	return s.engine.Name()
}

// instanceStatus maps the engine's report to the console's status words.
// Anything the engine has not picked up yet is Pending: committed, waiting.
func instanceStatus(inst gitops.Instance, engine repository.EngineStatus, engineName string) (string, string) {
	if !engine.Found {
		return repository.PhasePending, fmt.Sprintf("Committed to Git, waiting for %s to reconcile it.", engineName)
	}
	if engine.Phase == repository.PhaseReady && engine.Revision != "" && engine.Revision != inst.Version {
		return repository.PhaseUpdating, fmt.Sprintf("Moving from version %s to %s.", engine.Revision, inst.Version)
	}
	message := engine.Message
	if engine.Phase == repository.PhaseReady {
		message = ""
	}
	return engine.Phase, truncateMessage(message)
}

// boundConnections describes what an instance is wired to: the external
// connections its declaration layers in, and the connections of other
// instances its values name. Resolved says whether the connection exists: a
// file in Git for an external one, a deployed provider for an internal one.
func boundConnections(st gitops.InstanceState, externals map[string]bool, obs *projectObservation) []models.ServiceConnection {
	project := st.Instance.Project
	byName := map[string]models.ServiceConnection{}
	for _, name := range st.Instance.Connections {
		byName[name] = models.ServiceConnection{Name: name, Namespace: project, Kind: models.ConnectionKindExternal, Resolved: externals == nil || externals[name]}
	}
	if obs != nil {
		self := st.Instance.ReleaseName()
		walkStrings(st.Values, func(v string) {
			if _, seen := byName[v]; seen || v == self {
				return
			}
			if obs.outputs[v] {
				byName[v] = models.ServiceConnection{Name: v, Namespace: project, Kind: models.ConnectionKindInternal, Resolved: true}
			}
		})
	}
	if len(byName) == 0 {
		return nil
	}
	out := make([]models.ServiceConnection, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *DefaultServiceService) ListServices(ctx context.Context, project string) ([]models.ServiceInstance, error) {
	states, err := s.deployments.ListInstances(ctx, project)
	if err != nil {
		return nil, err
	}
	connections, err := s.deployments.ListConnections(ctx, project)
	if err != nil {
		return nil, err
	}
	externals := connectionNames(connections)
	obs := s.observe(ctx, project)

	result := make([]models.ServiceInstance, 0, len(states))
	for _, st := range states {
		release := st.Instance.ReleaseName()
		result = append(result, s.assemble(st, obs.engine[release], obs.descriptors[release], externals, &obs))
	}
	return result, nil
}

func (s *DefaultServiceService) GetService(ctx context.Context, project, name string) (*models.ServiceInstance, error) {
	st, err := s.deployments.GetInstance(ctx, project, name)
	if err != nil {
		return nil, gitError(err, name)
	}
	connections, err := s.deployments.ListConnections(ctx, project)
	if err != nil {
		return nil, err
	}
	obs := s.observe(ctx, project)
	release := st.Instance.ReleaseName()
	instance := s.assemble(*st, obs.engine[release], obs.descriptors[release], connectionNames(connections), &obs)
	// Only look for an explanation when the instance is not Ready and the
	// engine gave none: scanning events has a non-trivial API cost per request.
	if instance.Status != repository.PhaseReady && instance.Status != repository.PhasePending && instance.StatusMessage == "" {
		instance.StatusMessage = s.latestWarningMessage(ctx, project, release)
	}
	return &instance, nil
}

// latestWarningMessage scans Warning events in a namespace and returns the
// most recent message whose involvedObject belongs to the given release.
func (s *DefaultServiceService) latestWarningMessage(ctx context.Context, project, releaseName string) string {
	if project == "" || releaseName == "" || s.k8sClient == nil {
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
		if involvedName != releaseName && !strings.HasPrefix(involvedName, releaseName+"-") {
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

// --- Live stream ---

func (s *DefaultServiceService) WatchServices(ctx context.Context, project string) (<-chan ServiceEvent, error) {
	ctx, cancel := context.WithCancel(ctx)
	out := make(chan ServiceEvent, 16)

	type trigger struct {
		instance  string
		eventType string
	}
	triggers := make(chan trigger, 64)
	forward := func(instance, eventType string) {
		select {
		case triggers <- trigger{instance, eventType}:
		case <-ctx.Done():
		}
	}

	started := 0
	prefix := project + "-"
	if s.descriptors != nil {
		if w, err := s.descriptors.Watch(ctx, project); err != nil {
			logrus.WithError(err).Warn("Could not watch the instance descriptors")
		} else {
			started++
			go func() {
				defer w.Stop()
				defer cancel()
				for event := range w.ResultChan() {
					if cm, ok := event.Object.(*corev1.ConfigMap); ok {
						if release := cm.Labels[repository.LabelDescriptorInstance]; strings.HasPrefix(release, prefix) {
							forward(strings.TrimPrefix(release, prefix), string(event.Type))
						}
					}
				}
			}()
		}
	}
	if s.engine != nil {
		if w, err := s.engine.Watch(ctx); err != nil {
			logrus.WithError(err).WithField("engine", s.engine.Name()).Warn("Could not watch the GitOps engine objects")
		} else {
			started++
			go func() {
				defer w.Stop()
				defer cancel()
				for event := range w.ResultChan() {
					u, ok := event.Object.(*unstructured.Unstructured)
					if !ok {
						continue
					}
					if p, release := s.engine.ReleaseOf(u); p == project && strings.HasPrefix(release, prefix) {
						forward(strings.TrimPrefix(release, prefix), string(event.Type))
					}
				}
			}()
		}
	}
	commits, unsubscribe := s.changes.Subscribe(project)
	go func() {
		defer unsubscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-commits:
				forward(c.Instance, c.Type)
			}
		}
	}()
	if started == 0 {
		logrus.WithField("project", project).Warn("Streaming the services of a project from the console's own commits only")
	}

	go func() {
		defer close(out)
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-triggers:
				event, ok := s.eventFor(ctx, project, t.instance, t.eventType)
				if !ok {
					continue
				}
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// eventFor recomputes an instance after a change. The declaration decides
// whether it still exists: a descriptor deleted while Git still declares the
// instance is a change of status, not a deletion.
func (s *DefaultServiceService) eventFor(ctx context.Context, project, name, eventType string) (ServiceEvent, bool) {
	instance, err := s.GetService(ctx, project, name)
	if apierrors.IsNotFound(err) {
		return ServiceEvent{Type: "DELETED", Object: models.ServiceInstance{
			Name: name, ReleaseName: gitops.ReleaseName(project, name), TargetNamespace: project,
		}}, true
	}
	if err != nil {
		logrus.WithError(err).WithField("instance", project+"/"+name).Debug("Could not refresh a streamed instance")
		return ServiceEvent{}, false
	}
	if eventType != "ADDED" {
		eventType = "MODIFIED"
	}
	return ServiceEvent{Type: eventType, Object: *instance}, true
}

// --- Catalog (self-service) ---

func (s *DefaultServiceService) GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error) {
	return s.platformRepo.GetMenuCategories(ctx)
}

func (s *DefaultServiceService) GetIngressSuffix(ctx context.Context) (string, error) {
	return s.platformRepo.GetIngressSuffix(ctx)
}

func (s *DefaultServiceService) GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error) {
	return s.platformRepo.GetProfileImages(ctx)
}

// --- Pods ---

// instanceSelector selects the workloads of an instance: every one carries
// app.kubernetes.io/instance: <project>-<instance> (chart rules).
func instanceSelector(project, name string) string {
	return fmt.Sprintf("%s=%s", repository.LabelAppInstance, gitops.ReleaseName(project, name))
}

func (s *DefaultServiceService) ListPods(ctx context.Context, project, serviceName string) ([]models.Pod, error) {
	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	podList, err := s.k8sClient.Resource(podGVR).Namespace(project).List(ctx, metav1.ListOptions{
		LabelSelector: instanceSelector(project, serviceName),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	result := []models.Pod{}
	for _, item := range podList.Items {
		result = append(result, s.unstructuredToPod(&item))
	}
	return result, nil
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
// controller reclaims when the release goes.
//
// Deleting a volume is irreversible, so this fails closed: if the surviving
// releases cannot be listed, nothing is deleted rather than everything matching
// a prefix.
func (s *DefaultServiceService) cleanupUserResources(ctx context.Context, namespace, releaseName string) {
	if s.k8sClient == nil {
		return
	}
	// Called after the instance has been removed from Git, so this lists the neighbours.
	survivors, err := s.deployments.ListInstances(ctx, namespace)
	if err != nil {
		logrus.WithError(err).WithField("release", releaseName).
			Error("Skipping user resource cleanup: cannot tell this release's objects from its neighbours'")
		return
	}
	others := make([]string, 0, len(survivors))
	for i := range survivors {
		if name := survivors[i].Instance.ReleaseName(); name != releaseName {
			others = append(others, name)
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

func (s *DefaultServiceService) checkPodHealth(pods []unstructured.Unstructured, releaseName string, currentStatus string) string {
	for _, pod := range pods {
		if pod.GetLabels()[repository.LabelAppInstance] != releaseName {
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
	services, err := s.platformRepo.GetPlatformServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read platform services: %w", err)
	}

	for _, svc := range services {
		if svc.Name == name {
			return &svc, nil
		}
	}
	return nil, invalid("service %q is not available in the platform", name)
}

func (s *DefaultServiceService) validateParameters(ctx context.Context, serviceName, tag string, params map[string]any) error {
	// Filled by the platform and the connection layers, never by values.yaml.
	for _, key := range PlatformKeys {
		if _, set := params[key]; set {
			return invalid("parameter %q is reserved: the platform fills it", key)
		}
	}
	if s.schemaService == nil {
		return nil
	}

	schemaMap, err := s.schemaService.GetParameterSchema(ctx, serviceName, tag)
	if err != nil {
		return fmt.Errorf("could not fetch schema for %s@%s: %w", serviceName, tag, err)
	}

	if schemaMap == nil {
		// No schema at all: nothing to validate against. (A chart without
		// values.schema.json already fails the fetch above.)
		return nil
	}

	// From here on the chart has a schema: one that cannot be used fails the
	// request (fail closed) rather than committing values nobody checked.
	schemaJSON, err := json.Marshal(schemaMap)
	if err != nil {
		return fmt.Errorf("the schema of %s@%s cannot be encoded: %w", serviceName, tag, err)
	}

	compiler := jsonschema.NewCompiler()
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return fmt.Errorf("the schema of %s@%s cannot be read: %w", serviceName, tag, err)
	}
	if err := compiler.AddResource("schema.json", schemaDoc); err != nil {
		return fmt.Errorf("the schema of %s@%s cannot be loaded: %w", serviceName, tag, err)
	}

	sch, err := compiler.Compile("schema.json")
	if err != nil {
		return fmt.Errorf("the schema of %s@%s does not compile: %w", serviceName, tag, err)
	}

	if params == nil {
		// No parameter at all is an empty values file, not a null document.
		params = map[string]any{}
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
		return invalid("parameter validation failed: invalid parameters: %v", err)
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

func (s *DefaultServiceService) GetPodLogs(ctx context.Context, project, serviceName, podName, container string, tailLines int64, follow bool) (io.ReadCloser, error) {
	// The pod must be one ListPods shows for this instance. Without the check,
	// any pod of the namespace is readable through any service name, sidecars
	// and other tenants' jobs included.
	pod, err := s.typedClient.CoreV1().Pods(project).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, apierrors.NewNotFound(corev1.Resource("pods"), podName)
		}
		return nil, fmt.Errorf("failed to get pod: %w", err)
	}
	if pod.Labels[repository.LabelAppInstance] != gitops.ReleaseName(project, serviceName) {
		return nil, apierrors.NewNotFound(corev1.Resource("pods"), podName)
	}

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
