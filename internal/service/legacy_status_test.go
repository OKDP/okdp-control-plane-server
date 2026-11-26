package service

import (
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocd"
)

func TestLegacyStatusOfEachReleasePhase(t *testing.T) {
	cases := []struct {
		phase string
		want  string
	}{
		{"", "Installing"},
		{"READY", "Ready"},
		{"ERROR", "Error"},
		{"FAILED", "Error"},
		{"UPGRADING", "Updating"},
		{"INSTALLING", "Installing"},
		{"WAIT_OCI", "Installing"},
		{"WAIT_REPO", "Installing"},
		{"WAIT_HREL", "Installing"},
		{"WAIT_PRT", "Installing"},
		{"WAIT_OCNX", "Installing"},
		{"WAIT_DEPS", "Installing"},
		{"WAIT_ICNX", "Installing"},
		{"WAIT_COND", "Installing"},
		{"SUSPENDED", "Installing"},
		{"SOME_NEW_PHASE", "Installing"},
		{"ready", "Installing"},
	}
	for _, tc := range cases {
		if got := LegacyStatus(kubocd.Phase(tc.phase)); got != tc.want {
			t.Errorf("phase %q: status = %q, want %q", tc.phase, got, tc.want)
		}
	}
}
