package repository

import (
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

func TestPlatformServiceToMap(t *testing.T) {
	false_ := false
	true_ := true

	t.Run("serializes label and exposesUI when set", func(t *testing.T) {
		m := platformServiceToMap(models.PlatformService{
			Name:           "trino",
			Versions:       []string{"0.3.0", "0.2.0"},
			DefaultVersion: "0.3.0",
			Label:          "Trino SQL",
			ExposesUI:      &false_,
		})
		if m["label"] != "Trino SQL" {
			t.Errorf("label = %v, want %q", m["label"], "Trino SQL")
		}
		if m["exposesUI"] != false {
			t.Errorf("exposesUI = %v, want false", m["exposesUI"])
		}
		versions, ok := m["versions"].([]interface{})
		if !ok || len(versions) != 2 || versions[0] != "0.3.0" {
			t.Errorf("versions = %v, want [0.3.0 0.2.0]", m["versions"])
		}
		if m["default"] != "0.3.0" {
			t.Errorf("default = %v, want 0.3.0", m["default"])
		}
	})

	t.Run("omits label and exposesUI when unset", func(t *testing.T) {
		m := platformServiceToMap(models.PlatformService{
			Name:           "seaweedfs",
			Versions:       []string{"0.1.0"},
			DefaultVersion: "0.1.0",
		})
		if _, ok := m["label"]; ok {
			t.Errorf("label should be omitted when empty, got %v", m["label"])
		}
		if _, ok := m["exposesUI"]; ok {
			t.Errorf("exposesUI should be omitted when nil, got %v", m["exposesUI"])
		}
	})

	t.Run("writes exposesUI true explicitly", func(t *testing.T) {
		m := platformServiceToMap(models.PlatformService{
			Name:           "jupyterhub",
			Versions:       []string{"0.6.0"},
			DefaultVersion: "0.6.0",
			ExposesUI:      &true_,
		})
		if m["exposesUI"] != true {
			t.Errorf("exposesUI = %v, want true", m["exposesUI"])
		}
	})
}
