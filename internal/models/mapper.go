package models

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// projectDescriptionAnnot is the annotation carrying the project description
// on the Kubernetes Namespace that backs the project.
const projectDescriptionAnnot = "okdp.io/description"

// FromNamespaceToProject converts a Namespace to a Project model.
func FromNamespaceToProject(ns *corev1.Namespace) Project {
	return Project{
		Name:        ns.Name,
		Description: ns.Annotations[projectDescriptionAnnot],
	}
}

// FromUnstructuredToServiceInstance converts a KuboCD Release unstructured to a ServiceInstance model
func FromUnstructuredToServiceInstance(u *unstructured.Unstructured) ServiceInstance {
	serviceName := ""
	instanceName := ""
	if labels := u.GetLabels(); labels != nil {
		serviceName = labels["okdp.io/service"]
		instanceName = labels["okdp.io/instance-name"]
	}
	if instanceName == "" {
		instanceName = serviceName
	}

	tag, _, _ := unstructured.NestedString(u.Object, "spec", "package", "tag")
	targetNS, _, _ := unstructured.NestedString(u.Object, "spec", "targetNamespace")
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	roles, _, _ := unstructured.NestedStringSlice(u.Object, "status", "roles")

	status := MapPhaseToStatus(phase)

	params, _, _ := unstructured.NestedMap(u.Object, "spec", "parameters")
	typedParams := make(map[string]any)
	for k, v := range params {
		typedParams[k] = v
	}

	createdAt := ""
	if ts := u.GetCreationTimestamp(); !ts.IsZero() {
		createdAt = ts.Format("2006-01-02T15:04:05Z07:00")
	}

	return ServiceInstance{
		Name:            instanceName,
		ReleaseName:     u.GetName(),
		Service:         serviceName,
		ServiceTag:      tag,
		Status:          status,
		TargetNamespace: targetNS,
		Roles:           roles,
		Parameters:      typedParams,
		CreatedAt:       createdAt,
	}
}

// FromUnstructuredToSparkAppInstance converts a SparkApplication unstructured to a SparkAppInstance model
func FromUnstructuredToSparkAppInstance(u *unstructured.Unstructured) SparkAppInstance {
	appType, _, _ := unstructured.NestedString(u.Object, "spec", "type")
	mode, _, _ := unstructured.NestedString(u.Object, "spec", "mode")
	image, _, _ := unstructured.NestedString(u.Object, "spec", "image")
	state, _, _ := unstructured.NestedString(u.Object, "status", "applicationState", "state")
	errMsg, _, _ := unstructured.NestedString(u.Object, "status", "applicationState", "errorMessage")
	driverPod, _, _ := unstructured.NestedString(u.Object, "status", "driverInfo", "podName")

	executors := make(map[string]string)
	rawExec, found, _ := unstructured.NestedStringMap(u.Object, "status", "executorState")
	if found {
		executors = rawExec
	}

	createdAt := ""
	if ts := u.GetCreationTimestamp(); !ts.IsZero() {
		createdAt = ts.Format("2006-01-02T15:04:05Z07:00")
	}

	completedAt := ""
	if ann := u.GetAnnotations(); ann != nil {
		if v, ok := ann["sparkoperator.k8s.io/completed-at"]; ok {
			completedAt = v
		}
	}

	return SparkAppInstance{
		Name:          u.GetName(),
		Type:          appType,
		Mode:          mode,
		Image:         image,
		Status:        state,
		ErrorMessage:  errMsg,
		DriverPodName: driverPod,
		CreatedAt:     createdAt,
		CompletedAt:   completedAt,
		Executors:     executors,
	}
}

// MapPhaseToStatus normalizes a KuboCD Release.status.phase into a small set
// of UI-friendly statuses. First-install / wait phases (empty, INSTALLING,
// WAIT_HREL "waiting for Helm release", WAIT_PRT "waiting for parent",
// WAIT_OCI "waiting for OCI artifact") collapse into "Installing" so the
// Console shows a meaningful word instead of "Pending". UPGRADING stays as
// "Updating" to distinguish a re-deploy from a fresh install. Unknown phases
// fall back to "Installing" rather than leaking raw controller jargon.
func MapPhaseToStatus(phase string) string {
	switch phase {
	case "READY":
		return "Ready"
	case "ERROR", "FAILED":
		return "Error"
	case "UPGRADING":
		return "Updating"
	case "", "INSTALLING", "WAIT_HREL", "WAIT_PRT", "WAIT_OCI":
		return "Installing"
	default:
		return "Installing"
	}
}

