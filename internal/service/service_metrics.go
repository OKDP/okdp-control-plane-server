package service

import (
	"context"
	"fmt"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

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
		var pods []unstructured.Unstructured
		for _, pod := range allPods.Items {
			if pod.GetLabels()[repository.LabelAppInstance] == inst.ReleaseName {
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
	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	podList, err := s.k8sClient.Resource(podGVR).Namespace(project).List(ctx, metav1.ListOptions{
		LabelSelector: instanceSelector(project, serviceName),
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
