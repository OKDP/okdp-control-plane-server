package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func podFixture(name, phase string, containers ...map[string]any) unstructured.Unstructured {
	statuses := make([]any, 0, len(containers))
	for _, c := range containers {
		statuses = append(statuses, c)
	}
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"status":   map[string]any{"phase": phase, "containerStatuses": statuses},
	}}
}

func TestCheckPodHealth(t *testing.T) {
	s := &DefaultServiceService{}
	ready := map[string]any{"name": "c", "ready": true}
	starting := map[string]any{"name": "c", "ready": false}
	crash := map[string]any{"name": "c", "ready": false,
		"state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}}

	cases := []struct {
		name string
		pods []unstructured.Unstructured
		want string
	}{
		{"all ready", []unstructured.Unstructured{podFixture("app-1", "Running", ready)}, "Ready"},
		{"pending without container status", []unstructured.Unstructured{podFixture("app-1", "Pending")}, "Installing"},
		{"container not ready yet", []unstructured.Unstructured{podFixture("app-1", "Running", starting)}, "Installing"},
		{"crash wins over not ready", []unstructured.Unstructured{podFixture("app-1", "Running", starting), podFixture("app-2", "Running", crash)}, "Error"},
		{"completed job pod ignored", []unstructured.Unstructured{podFixture("app-job", "Succeeded", starting), podFixture("app-1", "Running", ready)}, "Ready"},
		{"unrelated pod ignored", []unstructured.Unstructured{podFixture("other-1", "Pending")}, "Ready"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, s.checkPodHealth(c.pods, []string{"app"}, "Ready"))
		})
	}
}

// Operators name pods after their own CR, not after the HelmRelease.
func TestCheckPodHealthMatchesOnInstanceLabel(t *testing.T) {
	s := &DefaultServiceService{}
	pod := podFixture("test-nifi-1-nodexyz", "Pending")
	pod.SetLabels(map[string]string{"app.kubernetes.io/instance": "test-nifi-main"})

	got := s.checkPodHealth([]unstructured.Unstructured{pod}, []string{"test-nifi-main"}, "Ready")
	assert.Equal(t, "Installing", got)
}
