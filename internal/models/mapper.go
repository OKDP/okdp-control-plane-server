package models

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

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
