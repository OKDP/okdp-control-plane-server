package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
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
	UpdateServiceParameters(ctx context.Context, project, name string, req models.ServiceUpdateRequest) (*models.ServiceInstance, error)
	DeleteService(ctx context.Context, project, name string) error
	WatchServices(ctx context.Context, project string) (watch.Interface, error)

	GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error)

	GetIngressSuffix(ctx context.Context) (string, error)

	GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error)

	EnrichURL(ctx context.Context, instance *models.ServiceInstance)
	EnrichPodHealth(ctx context.Context, instance *models.ServiceInstance)
	ListPods(ctx context.Context, project, serviceName string) ([]models.Pod, error)
	GetPodLogs(ctx context.Context, project, podName, container string, tailLines int64, follow bool) (io.ReadCloser, error)
	GetServiceMetrics(ctx context.Context, project, serviceName string) (*models.ServiceMetrics, error)
}

type DefaultServiceService struct {
	releaseRepo      repository.ServiceRepository
	contextRepo      repository.ContextRepository
	contextWriteRepo repository.ContextWriterRepository
	schemaService    PackageSchemaService
	oidcProvisioner  provisioning.OidcClientProvisioner
	k8sClient        dynamic.Interface
	typedClient      kubernetes.Interface
	contextNamespace string
	releaseInterval  string
	releaseTimeout   string
	sidecarPrefixes  []string
	// insecureRegistries mirrors INSECURE_OCI_REGISTRIES so the Releases this
	// service creates carry the insecure flag for those hosts.
	insecureRegistries []string
}

// SetInsecureRegistries declares the plain-HTTP registry hosts.
func (s *DefaultServiceService) SetInsecureRegistries(hosts []string) {
	s.insecureRegistries = hosts
}

func NewDefaultServiceService(releaseRepo repository.ServiceRepository, contextRepo repository.ContextRepository, contextWriteRepo repository.ContextWriterRepository, schemaService PackageSchemaService, oidcProvisioner provisioning.OidcClientProvisioner, k8sClient dynamic.Interface, typedClient kubernetes.Interface, contextNamespace, releaseInterval, releaseTimeout string, sidecarPrefixes []string) *DefaultServiceService {
	return &DefaultServiceService{
		releaseRepo:      releaseRepo,
		contextRepo:      contextRepo,
		contextWriteRepo: contextWriteRepo,
		schemaService:    schemaService,
		oidcProvisioner:  oidcProvisioner,
		k8sClient:        k8sClient,
		typedClient:      typedClient,
		contextNamespace: contextNamespace,
		releaseInterval:  releaseInterval,
		releaseTimeout:   releaseTimeout,
		sidecarPrefixes:  sidecarPrefixes,
	}
}

// --- Platform services ---

func (s *DefaultServiceService) GetPlatformServices(ctx context.Context) ([]models.PlatformService, error) {
	services, err := s.contextRepo.GetPlatformServices(ctx)
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

// AddPlatformService validates and appends a new service to the catalog (default Context).
func (s *DefaultServiceService) AddPlatformService(ctx context.Context, svc models.PlatformService) (*models.PlatformService, error) {
	if s.contextWriteRepo == nil {
		return nil, fmt.Errorf("catalog management is not available")
	}
	if err := normalizeAndValidateCatalogService(&svc); err != nil {
		return nil, err
	}

	existing, err := s.contextRepo.GetPlatformServices(ctx)
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

	if err := s.contextWriteRepo.AddPlatformService(ctx, svc); err != nil {
		return nil, err
	}
	s.contextRepo.Invalidate()
	return &svc, nil
}

// UpdatePlatformService validates and replaces an existing catalog service.
func (s *DefaultServiceService) UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) (*models.PlatformService, error) {
	if s.contextWriteRepo == nil {
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

	if err := s.contextWriteRepo.UpdatePlatformService(ctx, name, svc); err != nil {
		return nil, err
	}
	s.contextRepo.Invalidate()
	return &svc, nil
}

// RemovePlatformService removes a service from the catalog.
func (s *DefaultServiceService) RemovePlatformService(ctx context.Context, name string) error {
	if s.contextWriteRepo == nil {
		return fmt.Errorf("catalog management is not available")
	}
	if _, err := s.findCatalogService(ctx, name); err != nil {
		return err
	}
	if err := s.contextWriteRepo.RemovePlatformService(ctx, name); err != nil {
		return err
	}
	s.contextRepo.Invalidate()
	return nil
}

// findCatalogService returns the catalog service by name or ErrCatalogServiceNotFound.
func (s *DefaultServiceService) findCatalogService(ctx context.Context, name string) (*models.PlatformService, error) {
	existing, err := s.contextRepo.GetPlatformServices(ctx)
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
		return nil, fmt.Errorf("parameter validation failed: %w", err)
	}

	packageRepo, err := s.contextRepo.GetPackageRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve package repository: %w", err)
	}
	if svc.Repository != "" {
		packageRepo = svc.Repository
	}

	ingressSuffix, err := s.contextRepo.GetIngressSuffix(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve ingress suffix: %w", err)
	}

	instanceName := req.InstanceName
	if instanceName == "" {
		instanceName = req.Service
	}
	releaseName := fmt.Sprintf("%s-%s", project, instanceName)

	release := &crd.Release{
		TypeMeta: metav1.TypeMeta{
			APIVersion: crd.ReleaseAPIVersion,
			Kind:       crd.ReleaseKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      releaseName,
			Namespace: project,
			Labels: map[string]string{
				crd.LabelProject:      project,
				crd.LabelService:      req.Service,
				crd.LabelInstanceName: instanceName,
			},
		},
		Spec: crd.ReleaseSpec{
			Description: fmt.Sprintf("%s for project %s", req.Service, project),
			Package: crd.ReleasePackage{
				Repository: fmt.Sprintf("%s/%s", packageRepo, req.Service),
				Tag:        deployTag,
				Interval:   s.releaseInterval,
				Timeout:    s.releaseTimeout,
				Insecure:   insecureOCIHost(packageRepo, s.insecureRegistries),
			},
			Parameters: req.Parameters,
			// No explicit Contexts. KuboCD merges Config.defaultContexts, then the
			// optional Context named by Config.defaultNamespaceContexts looked up in
			// this Release's namespace. A project overriding nothing needs no object.
			TargetNamespace: project,
			CreateNamespace: false,
		},
	}

	if err := s.releaseRepo.Create(ctx, project, release); err != nil {
		return nil, err
	}

	instance := releaseToInstance(release)
	instance.URL = fmt.Sprintf("https://%s.%s", releaseName, ingressSuffix)
	return instance, nil
}

func (s *DefaultServiceService) ListServices(ctx context.Context, project string) ([]models.ServiceInstance, error) {
	releases, err := s.releaseRepo.List(ctx, project, project)
	if err != nil {
		return nil, err
	}

	var result []models.ServiceInstance
	for i := range releases {
		result = append(result, *releaseToInstance(&releases[i]))
	}
	s.enrichWithURL(ctx, result)
	s.enrichWithPodHealth(ctx, result)
	return result, nil
}

func (s *DefaultServiceService) GetService(ctx context.Context, project, name string) (*models.ServiceInstance, error) {
	releaseName := fmt.Sprintf("%s-%s", project, name)
	release, err := s.releaseRepo.Get(ctx, project, releaseName)
	if err != nil {
		return nil, err
	}
	instance := releaseToInstance(release)
	s.setURLIfExposed(ctx, instance)
	instances := []models.ServiceInstance{*instance}
	s.enrichWithPodHealth(ctx, instances)
	instance.Status = instances[0].Status
	// Only surface an explanatory message when the instance is not Ready: there
	// is nothing useful to show on a healthy service, and scanning events has a
	// non-trivial API cost per request.
	if instance.Status != "Ready" {
		instance.StatusMessage = s.latestWarningMessage(ctx, instance.TargetNamespace, instance.ReleaseName)
	}
	return instance, nil
}

// latestWarningMessage scans Warning events in a namespace and returns the
// most recent message whose involvedObject name is related to the given
// release (prefix match). Used to turn a stuck KuboCD release into an
// actionable UI error like "Deployment.apps is invalid: memory request must
// be less than or equal to memory limit of 1". Returns "" if no relevant
// event exists or the API call fails, so callers should treat empty as "no
// additional context available".
func (s *DefaultServiceService) latestWarningMessage(ctx context.Context, namespace, releaseName string) string {
	if namespace == "" || releaseName == "" {
		return ""
	}
	eventsGVR := schema.GroupVersionResource{Version: "v1", Resource: "events"}
	events, err := s.k8sClient.Resource(eventsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "type=Warning",
	})
	if err != nil {
		logrus.WithError(err).Debugf("latestWarningMessage: event list failed in %s", namespace)
		return ""
	}

	var (
		latestAt  time.Time
		latestMsg string
	)
	for _, e := range events.Items {
		involvedName, _, _ := unstructured.NestedString(e.Object, "involvedObject", "name")
		// HelmReleases and deployed workloads are named "<releaseName>-*"
		// (e.g. "redjohn-jupyterhub-main", "redjohn-jupyterhub-hub").
		if involvedName == "" || !strings.HasPrefix(involvedName, releaseName) {
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

func (s *DefaultServiceService) DeleteService(ctx context.Context, project, name string) error {
	releaseName := fmt.Sprintf("%s-%s", project, name)

	if err := s.releaseRepo.Delete(ctx, project, releaseName); err != nil {
		return err
	}

	s.cleanupOidcClient(ctx, releaseName)
	s.cleanupUserResources(ctx, project, releaseName)
	return nil
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
	survivors, err := s.releaseRepo.List(ctx, namespace, namespace)
	if err != nil {
		logrus.WithError(err).WithField("release", releaseName).
			Error("Skipping user resource cleanup: cannot tell this release's objects from its neighbours'")
		return
	}
	others := make([]string, 0, len(survivors))
	for i := range survivors {
		if survivors[i].Name != releaseName {
			others = append(others, survivors[i].Name)
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

func (s *DefaultServiceService) WatchServices(ctx context.Context, project string) (watch.Interface, error) {
	return s.releaseRepo.Watch(ctx, project, project)
}

// --- Catalog (self-service) ---

func (s *DefaultServiceService) GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error) {
	return s.contextRepo.GetMenuCategories(ctx)
}

func (s *DefaultServiceService) GetIngressSuffix(ctx context.Context) (string, error) {
	return s.contextRepo.GetIngressSuffix(ctx)
}

func (s *DefaultServiceService) GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error) {
	return s.contextRepo.GetProfileImages(ctx)
}

func (s *DefaultServiceService) UpdateServiceParameters(ctx context.Context, project, name string, req models.ServiceUpdateRequest) (*models.ServiceInstance, error) {
	releaseName := fmt.Sprintf("%s-%s", project, name)

	release, err := s.releaseRepo.Get(ctx, project, releaseName)
	if err != nil {
		return nil, err
	}

	if req.Tag != "" {
		release.Spec.Package.Tag = req.Tag
	}

	if req.Parameters != nil {
		if release.Spec.Parameters == nil {
			release.Spec.Parameters = make(map[string]any)
		}
		for k, v := range req.Parameters {
			release.Spec.Parameters[k] = v
		}
	}

	serviceName := release.Labels[crd.LabelService]
	if err := s.validateParameters(ctx, serviceName, release.Spec.Package.Tag, release.Spec.Parameters); err != nil {
		return nil, fmt.Errorf("parameter validation failed: %w", err)
	}

	if err := s.releaseRepo.Update(ctx, project, release); err != nil {
		return nil, fmt.Errorf("failed to update release: %w", err)
	}

	instance := releaseToInstance(release)
	s.setURLIfExposed(ctx, instance)
	return instance, nil
}

// --- helpers ---

func (s *DefaultServiceService) enrichWithURL(ctx context.Context, instances []models.ServiceInstance) {
	suffix, err := s.contextRepo.GetIngressSuffix(ctx)
	if err != nil {
		return
	}
	// Only expose a URL when an Ingress actually routes to the service. A
	// service with no web UI (e.g. an operator) gets no URL, so the UI shows
	// no dead "Open" link. Ingresses are listed once per namespace and reused
	// across that namespace's instances.
	hostsByNS := map[string]map[string]bool{}
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
		for _, host := range candidateHosts(&instances[i], suffix) {
			if hosts[host] {
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
	suffix, err := s.contextRepo.GetIngressSuffix(ctx)
	if err != nil || instance == nil || instance.TargetNamespace == "" {
		return
	}
	hosts := s.namespaceIngressHosts(ctx, instance.TargetNamespace)
	for _, host := range candidateHosts(instance, suffix) {
		if hosts[host] {
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
// convention that applies to it, then the platform-packages catalog's own
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
	if instance.Service != "" {
		hosts = append(hosts,
			fmt.Sprintf("%s-console-%s.%s", instance.Service, instance.TargetNamespace, suffix),
			fmt.Sprintf("%s-%s.%s", instance.Service, instance.TargetNamespace, suffix),
		)
	}
	return hosts
}

// namespaceIngressHosts returns the set of hosts served by Ingresses in a
// namespace (empty on error, so a lookup failure never fabricates a URL).
func (s *DefaultServiceService) namespaceIngressHosts(ctx context.Context, namespace string) map[string]bool {
	ingressGVR := schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
	list, err := s.k8sClient.Resource(ingressGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return map[string]bool{}
	}
	return ingressHostsFromItems(list.Items)
}

// ingressHostsFromItems extracts the set of spec.rules[].host values from a
// list of Ingress objects.
func ingressHostsFromItems(items []unstructured.Unstructured) map[string]bool {
	hosts := map[string]bool{}
	for i := range items {
		rules, _, _ := unstructured.NestedSlice(items[i].Object, "spec", "rules")
		for _, r := range rules {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			if host, ok := rm["host"].(string); ok && host != "" {
				hosts[host] = true
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

		prefix := instances[i].ReleaseName + "-"
		instances[i].Status = s.checkPodHealth(pods, prefix, instances[i].Status)
	}
}

func (s *DefaultServiceService) EnrichURL(ctx context.Context, instance *models.ServiceInstance) {
	s.setURLIfExposed(ctx, instance)
}

func (s *DefaultServiceService) EnrichPodHealth(ctx context.Context, instance *models.ServiceInstance) {
	if instance == nil || instance.Status != "Ready" || instance.TargetNamespace == "" {
		return
	}
	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	podList, err := s.k8sClient.Resource(podGVR).Namespace(instance.TargetNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	prefix := instance.ReleaseName + "-"
	instance.Status = s.checkPodHealth(podList.Items, prefix, instance.Status)
}

func (s *DefaultServiceService) checkPodHealth(pods []unstructured.Unstructured, prefix, currentStatus string) string {
	for _, pod := range pods {
		if !strings.HasPrefix(pod.GetName(), prefix) {
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
	services, err := s.contextRepo.GetPlatformServices(ctx)
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

func (s *DefaultServiceService) validateParameters(ctx context.Context, serviceName, tag string, params map[string]any) error {
	if s.schemaService == nil {
		return nil
	}

	schemaMap, err := s.schemaService.GetParameterSchema(ctx, serviceName, tag)
	if err != nil {
		return fmt.Errorf("could not fetch schema for %s@%s: %w", serviceName, tag, err)
	}

	schemaJSON, err := json.Marshal(schemaMap)
	if err != nil {
		return nil
	}

	compiler := jsonschema.NewCompiler()
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		logrus.WithError(err).Warn("Failed to unmarshal schema JSON, skipping validation")
		return nil
	}
	if err := compiler.AddResource("schema.json", schemaDoc); err != nil {
		logrus.WithError(err).Warn("Failed to add schema resource, skipping validation")
		return nil
	}

	sch, err := compiler.Compile("schema.json")
	if err != nil {
		logrus.WithError(err).Warn("Failed to compile schema, skipping validation")
		return nil
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
		return fmt.Errorf("invalid parameters: %w", err)
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

func (s *DefaultServiceService) ListPods(ctx context.Context, project, serviceName string) ([]models.Pod, error) {
	releaseName := fmt.Sprintf("%s-%s", project, serviceName)

	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	podList, err := s.k8sClient.Resource(podGVR).Namespace(project).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.kubernetes.io/instance=%s", releaseName),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Fallback to prefix matching if label selector returns nothing
	if len(podList.Items) == 0 {
		allPods, err := s.k8sClient.Resource(podGVR).Namespace(project).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list pods (fallback): %w", err)
		}
		prefix := releaseName + "-"
		for _, pod := range allPods.Items {
			if strings.HasPrefix(pod.GetName(), prefix) {
				podList.Items = append(podList.Items, pod)
			}
		}
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

func releaseToInstance(r *crd.Release) *models.ServiceInstance {
	status := models.MapPhaseToStatus(r.Status.Phase)

	serviceName := r.Labels[crd.LabelService]
	instanceName := r.Labels[crd.LabelInstanceName]
	if instanceName == "" {
		instanceName = serviceName
	}

	createdAt := ""
	if !r.CreationTimestamp.IsZero() {
		createdAt = r.CreationTimestamp.Format(time.RFC3339)
	}

	return &models.ServiceInstance{
		Name:            instanceName,
		ReleaseName:     r.Name,
		Service:         serviceName,
		ServiceTag:      r.Spec.Package.Tag,
		Status:          status,
		TargetNamespace: r.Spec.TargetNamespace,
		Roles:           r.Status.Roles,
		Parameters:      r.Spec.Parameters,
		Connections:     boundConnections(r),
		CreatedAt:       createdAt,
	}
}

// boundConnections describes what a release is wired to, from what the
// controller published.
//
// The two lists do not mean what their names suggest. A connectionRef with no
// kind resolves by looking on both sides, so watchedInputConnections holds
// every *candidate*, a Connection and a ClusterConnection per name, not a set
// of pending bindings. Listing them as they come showed "demo-db
// (ClusterConnection) waiting" next to the Connection that had resolved, for a
// ClusterConnection that never existed. Candidates are therefore grouped by
// name: a name that resolved shows once, resolved. A name that resolved nowhere
// shows once, waiting, which is the case a reader opens this page for.
func boundConnections(r *crd.Release) []models.ServiceConnection {
	byName := map[string]models.ServiceConnection{}
	order := make([]string, 0, len(r.Status.WatchedInputConnections))

	remember := func(ref crd.InputConnectionReference, resolved bool) {
		current, seen := byName[ref.Name]
		if !seen {
			order = append(order, ref.Name)
		}
		// A resolved candidate always wins over a pending one.
		if seen && current.Resolved {
			return
		}
		byName[ref.Name] = models.ServiceConnection{
			Name: ref.Name, Namespace: ref.Namespace, Kind: ref.Kind, Resolved: resolved,
		}
	}

	for _, ref := range r.Status.EffectiveInputConnections {
		remember(ref, true)
	}
	for _, ref := range r.Status.WatchedInputConnections {
		remember(ref, false)
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
func (s *DefaultServiceService) GetServiceMetrics(ctx context.Context, project, serviceName string) (*models.ServiceMetrics, error) {
	releaseName := fmt.Sprintf("%s-%s", project, serviceName)

	// 1. Fetch the pods of the service (same selection logic as ListPods).
	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	podList, err := s.k8sClient.Resource(podGVR).Namespace(project).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.kubernetes.io/instance=%s", releaseName),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	if len(podList.Items) == 0 {
		allPods, err := s.k8sClient.Resource(podGVR).Namespace(project).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list pods (fallback): %w", err)
		}
		prefix := releaseName + "-"
		for _, pod := range allPods.Items {
			if strings.HasPrefix(pod.GetName(), prefix) {
				podList.Items = append(podList.Items, pod)
			}
		}
	}

	var cpuLimit, memLimit float64
	var cpuUsed, memUsed float64
	cpuUsedAvailable := false
	memUsedAvailable := false

	// 2. Sum CPU/memory limits from the pod specs.
	for _, pod := range podList.Items {
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
			// Prefer limits, fall back to requests.
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
				break // only count one bucket per container
			}
		}
	}

	// 3. Query metrics.k8s.io for live usage, once for the namespace.
	metricsGVR := schema.GroupVersionResource{
		Group:    "metrics.k8s.io",
		Version:  "v1beta1",
		Resource: "pods",
	}
	metricsList, err := s.k8sClient.Resource(metricsGVR).Namespace(project).List(ctx, metav1.ListOptions{})
	if err != nil {
		logrus.Debugf("metrics for namespace %s unavailable: %v", project, err)
		metricsList = &unstructured.UnstructuredList{}
	}
	metricsByPod := make(map[string]*unstructured.Unstructured, len(metricsList.Items))
	for i := range metricsList.Items {
		metricsByPod[metricsList.Items[i].GetName()] = &metricsList.Items[i]
	}

	for _, pod := range podList.Items {
		podMetrics, ok := metricsByPod[pod.GetName()]
		if !ok {
			logrus.Debugf("metrics for pod %s/%s unavailable", project, pod.GetName())
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
	return metrics, nil
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
