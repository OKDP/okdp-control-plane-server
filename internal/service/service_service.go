package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/okdp/okdp-server-new/internal/models"
	"github.com/okdp/okdp-server-new/internal/repository"
	"github.com/okdp/okdp-server-new/internal/repository/crd"
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

// ServiceService manages platform services (deploy, monitor, delete) and exposes the catalog.
type ServiceService interface {
	GetPlatformServices(ctx context.Context) ([]models.PlatformService, error)
	DeployService(ctx context.Context, project string, req models.ServiceRequest) (*models.ServiceInstance, error)
	ListServices(ctx context.Context, project string) ([]models.ServiceInstance, error)
	GetService(ctx context.Context, project, name string) (*models.ServiceInstance, error)
	UpdateServiceParameters(ctx context.Context, project, name string, req models.ServiceUpdateRequest) (*models.ServiceInstance, error)
	DeleteService(ctx context.Context, project, name string) error
	WatchServices(ctx context.Context, project string) (watch.Interface, error)

	GetCatalog(ctx context.Context) ([]models.CatalogCategory, error)

	GetIngressSuffix(ctx context.Context) (string, error)

	GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error)

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
	k8sClient        dynamic.Interface
	typedClient      kubernetes.Interface
	contextNamespace string
	releaseInterval  string
	releaseTimeout   string
	sidecarPrefixes  []string
}

func NewDefaultServiceService(releaseRepo repository.ServiceRepository, contextRepo repository.ContextRepository, contextWriteRepo repository.ContextWriterRepository, schemaService PackageSchemaService, k8sClient dynamic.Interface, typedClient kubernetes.Interface, contextNamespace, releaseInterval, releaseTimeout string, sidecarPrefixes []string) *DefaultServiceService {
	return &DefaultServiceService{
		releaseRepo:      releaseRepo,
		contextRepo:      contextRepo,
		contextWriteRepo: contextWriteRepo,
		schemaService:    schemaService,
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

	for i, svc := range services {
		versionsResp, err := s.schemaService.GetServiceVersions(ctx, svc.Name)
		if err != nil {
			logrus.WithError(err).Warnf("failed to fetch OCI versions for %s", svc.Name)
			continue
		}
		services[i].Versions = versionsResp.Versions
		if versionsResp.Default != "" {
			services[i].DefaultVersion = versionsResp.Default
		}
	}

	return services, nil
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

	if s.contextWriteRepo != nil {
		if err := s.contextWriteRepo.SyncFromDefault(ctx, project); err != nil {
			logrus.WithError(err).WithField("project", project).Warn("Failed to sync project Context from default")
		}
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
			},
			Parameters: req.Parameters,
			Contexts: []crd.ContextRef{
				{Name: project, Namespace: s.contextNamespace},
			},
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
	if suffix, err := s.contextRepo.GetIngressSuffix(ctx); err == nil {
		instance.URL = fmt.Sprintf("https://%s.%s", instance.ReleaseName, suffix)
	}
	instances := []models.ServiceInstance{*instance}
	s.enrichWithPodHealth(ctx, instances)
	instance.Status = instances[0].Status
	// Only surface an explanatory message when the instance is not Ready — there
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
// event exists or the API call fails — callers should treat empty as "no
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
// several hundred characters — we cap to 400 so the UI does not blow up.
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

var oidcClientGVR = schema.GroupVersionResource{
	Group:    "kubauth.kubotal.io",
	Version:  "v1alpha1",
	Resource: "oidcclients",
}

func (s *DefaultServiceService) cleanupOidcClient(ctx context.Context, releaseName string) {
	kubauthNS, err := s.contextRepo.GetKubauthNamespace(ctx)
	if err != nil {
		logrus.WithError(err).Debug("Could not resolve kubauth namespace for OidcClient cleanup")
		return
	}

	err = s.k8sClient.Resource(oidcClientGVR).Namespace(kubauthNS).Delete(ctx, releaseName, metav1.DeleteOptions{})
	if err != nil {
		logrus.WithError(err).WithField("oidcClient", releaseName).Debug("OidcClient cleanup skipped")
	} else {
		logrus.WithField("oidcClient", releaseName).Info("Cleaned up OidcClient")
	}
}

func (s *DefaultServiceService) cleanupUserResources(ctx context.Context, namespace, releaseName string) {
	prefix := releaseName + "-"

	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	pods, err := s.k8sClient.Resource(podGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, pod := range pods.Items {
			if len(pod.GetName()) > len(prefix) && pod.GetName()[:len(prefix)] == prefix {
				_ = s.k8sClient.Resource(podGVR).Namespace(namespace).Delete(ctx, pod.GetName(), metav1.DeleteOptions{})
				logrus.WithField("pod", pod.GetName()).Info("Cleaned up user pod")
			}
		}
	}

	pvcGVR := schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	pvcs, err := s.k8sClient.Resource(pvcGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, pvc := range pvcs.Items {
			if len(pvc.GetName()) > len(prefix) && pvc.GetName()[:len(prefix)] == prefix {
				_ = s.k8sClient.Resource(pvcGVR).Namespace(namespace).Delete(ctx, pvc.GetName(), metav1.DeleteOptions{})
				logrus.WithField("pvc", pvc.GetName()).Info("Cleaned up user PVC")
			}
		}
	}
}

func (s *DefaultServiceService) WatchServices(ctx context.Context, project string) (watch.Interface, error) {
	return s.releaseRepo.Watch(ctx, project, project)
}

// --- Catalog (self-service) ---

func (s *DefaultServiceService) GetCatalog(ctx context.Context) ([]models.CatalogCategory, error) {
	return s.contextRepo.GetCatalog(ctx)
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
	if suffix, err := s.contextRepo.GetIngressSuffix(ctx); err == nil {
		instance.URL = fmt.Sprintf("https://%s.%s", instance.ReleaseName, suffix)
	}
	return instance, nil
}

// --- helpers ---

func (s *DefaultServiceService) enrichWithURL(ctx context.Context, instances []models.ServiceInstance) {
	suffix, err := s.contextRepo.GetIngressSuffix(ctx)
	if err != nil {
		return
	}
	for i := range instances {
		instances[i].URL = fmt.Sprintf("https://%s.%s", instances[i].ReleaseName, suffix)
	}
}

// enrichWithPodHealth overrides a "Ready" Release status when pods are actually unhealthy.
func (s *DefaultServiceService) enrichWithPodHealth(ctx context.Context, instances []models.ServiceInstance) {
	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}

	for i := range instances {
		if instances[i].Status != "Ready" {
			continue
		}
		ns := instances[i].TargetNamespace
		if ns == "" {
			continue
		}
		podList, err := s.k8sClient.Resource(podGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}

		prefix := instances[i].ReleaseName + "-"
		instances[i].Status = s.checkPodHealth(podList.Items, prefix, instances[i].Status)
	}
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
		Parameters:      r.Spec.Parameters,
		CreatedAt:       createdAt,
	}
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

	// 3. Query metrics.k8s.io for live usage, per pod.
	metricsGVR := schema.GroupVersionResource{
		Group:    "metrics.k8s.io",
		Version:  "v1beta1",
		Resource: "pods",
	}
	for _, pod := range podList.Items {
		podMetrics, err := s.k8sClient.Resource(metricsGVR).Namespace(project).Get(ctx, pod.GetName(), metav1.GetOptions{})
		if err != nil {
			// metrics-server may not yet have a sample for a brand-new pod; skip it.
			logrus.Debugf("metrics for pod %s/%s unavailable: %v", project, pod.GetName(), err)
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
// k8s.io/apimachinery resource.ParseQuantity — the canonical parser used
// throughout the Kubernetes ecosystem — so we inherit correct handling of
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
// rationale as parseCPUQuantity — we delegate to resource.ParseQuantity.
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
