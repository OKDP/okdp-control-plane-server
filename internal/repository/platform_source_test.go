package repository

import (
	"context"
	"strings"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"

	"k8s.io/client-go/kubernetes/fake"
)

func TestMissingKeysNameThePlatformValues(t *testing.T) {
	repo := newContextWith(t, map[string]interface{}{})

	cases := []struct {
		name string
		call func() error
	}{
		{"ingress.suffix", func() error {
			_, err := repo.GetIngressSuffix(context.Background())
			return err
		}},
		{"sparkOperator", func() error {
			_, err := repo.GetSparkConfig(context.Background())
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("expected an error naming the platform values")
			}
			if !strings.Contains(err.Error(), "global.okdp") {
				t.Errorf("expected global.okdp to be named, got %q", err.Error())
			}
		})
	}
}

// Argo CD creates no values ConfigMap: the platform values are then read from
// Git, where both engines take them from.
func TestPlatformValuesFallBackToGit(t *testing.T) {
	store := gitops.NewMemoryStore(map[string]string{
		gitops.PlatformValuesPath: "global:\n  okdp:\n    ingress:\n      suffix: okdp.sandbox\n",
	})
	repo := NewPlatformRepository(fake.NewSimpleClientset(), gitops.ReleasesNamespace, gitops.NewDeployments(store, nil))
	suffix, err := repo.GetIngressSuffix(context.Background())
	if err != nil || suffix != "okdp.sandbox" {
		t.Fatalf("suffix = %q, err = %v", suffix, err)
	}
}

func TestNoPlatformValuesAtAllIsAnError(t *testing.T) {
	repo := NewPlatformRepository(fake.NewSimpleClientset(), gitops.ReleasesNamespace, gitops.NewDeployments(gitops.NewMemoryStore(nil), nil))
	if _, err := repo.GetIngressSuffix(context.Background()); err == nil || !strings.Contains(err.Error(), gitops.PlatformValuesConfigMap) {
		t.Fatalf("err = %v, want one naming the ConfigMap", err)
	}
}
