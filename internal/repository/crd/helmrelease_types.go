package crd

import "k8s.io/apimachinery/pkg/runtime/schema"

func GetHelmReleaseGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "helm.toolkit.fluxcd.io",
		Version:  "v2",
		Resource: "helmreleases",
	}
}
