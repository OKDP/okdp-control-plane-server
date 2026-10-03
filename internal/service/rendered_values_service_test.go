package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

type stubValuesFetcher struct {
	values map[string][]byte
	err    error
	calls  []string
}

func (f *stubValuesFetcher) FetchVendoredValues(_ context.Context, repository, tag string, plainHTTP bool) (map[string][]byte, error) {
	f.calls = append(f.calls, repository+":"+tag)
	return f.values, f.err
}

func valuesConfigMap(name, release, chart, helmChart, values string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "demo",
			Labels: map[string]string{
				repository.LabelVendorValues: chart,
				repository.LabelAppInstance:  release,
				repository.LabelHelmChart:    helmChart,
			},
			Annotations: map[string]string{repository.AnnotationVendorChart: chart + "-1.0.0"},
		},
		Data: map[string]string{"values.yaml": values},
	}
}

func newRenderedValuesUnderTest(t *testing.T, fetcher ChartValuesFetcher, objects ...*corev1.ConfigMap) *DefaultRenderedValuesService {
	t.Helper()
	store := gitops.NewMemoryStore(map[string]string{
		"projects/demo/project.yaml":                     "name: demo\n",
		"projects/demo/services/sql/instance.yaml":       "name: sql\nproject: demo\nservice: trino\nchart: oci://quay.io/okdp/platform-charts/trino\nversion: 480.0.0-1.0.2\nconnections: []\n",
		"projects/demo/services/sql/values.yaml":         "{}\n",
		"projects/demo/services/sql-other/instance.yaml": "name: sql-other\nproject: demo\nservice: trino\nchart: oci://quay.io/okdp/platform-charts/trino\nversion: 480.0.0-1.0.2\nconnections: []\n",
		"projects/demo/services/sql-other/values.yaml":   "{}\n",
	})
	client := k8sfake.NewSimpleClientset()
	for _, cm := range objects {
		_, err := client.CoreV1().ConfigMaps(cm.Namespace).Create(context.Background(), cm, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	svc := NewDefaultRenderedValuesService(gitops.NewDeployments(store, nil), repository.NewVendorValuesRepository(client), nil)
	svc.SetChartFetcher(fetcher)
	return svc
}

func TestRenderedValuesMarksTheChangesAgainstTheVendoredDefaults(t *testing.T) {
	fetcher := &stubValuesFetcher{values: map[string][]byte{
		"trino":         []byte("# defaults\nworkers: 1\nimage: trinodb/trino\n"),
		"opa-kube-mgmt": []byte("replicas: 1\n"),
	}}
	svc := newRenderedValuesUnderTest(t, fetcher,
		valuesConfigMap("demo-sql-opa-kube-mgmt-values", "demo-sql", "opa-kube-mgmt", "trino-480.0.0-1.0.3", "replicas: 1\n"),
		valuesConfigMap("demo-sql-trino-values", "demo-sql", "trino", "trino-480.0.0-1.0.3", "image: trinodb/trino\nworkers: 3\n"),
		// Another release of the namespace (demo-sql-other starts with demo-sql).
		valuesConfigMap("demo-sql-other-trino-values", "demo-sql-other", "trino", "trino-480.0.0-1.0.3", "workers: 9\n"),
	)

	got, err := svc.GetRenderedValues(context.Background(), "demo", "sql")
	require.NoError(t, err)
	require.Len(t, got, 2)
	// The instance's own chart first.
	require.Equal(t, "trino", got[0].Chart)
	require.Equal(t, "demo-sql-trino-values", got[0].Name)
	require.Equal(t, "trino-1.0.0", got[0].ChartVersion)
	require.Equal(t, []int{2}, got[0].ChangedLines)
	require.Contains(t, got[0].Defaults, "# defaults")
	require.Empty(t, got[0].DefaultsError)
	require.Equal(t, "opa-kube-mgmt", got[1].Chart)
	require.Equal(t, []int{}, got[1].ChangedLines)
	// The version that rendered the ConfigMaps (helm.sh/chart), not the
	// declared one, pulled once for both renders.
	require.Equal(t, "480.0.0-1.0.3", got[0].ServiceVersion)
	require.Equal(t, []string{"quay.io/okdp/platform-charts/trino:480.0.0-1.0.3"}, fetcher.calls)
}

func TestRenderedValuesWithoutDefaults(t *testing.T) {
	cm := valuesConfigMap("demo-sql-trino-values", "demo-sql", "trino", "", "workers: 3\n")

	unreachable := newRenderedValuesUnderTest(t, &stubValuesFetcher{err: errors.New("registry down")}, cm)
	got, err := unreachable.GetRenderedValues(context.Background(), "demo", "sql")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "workers: 3\n", got[0].Values)
	require.Equal(t, []int{}, got[0].ChangedLines)
	require.Contains(t, got[0].DefaultsError, "registry down")
	// No helm.sh/chart label: the declared version.
	require.Equal(t, "480.0.0-1.0.2", got[0].ServiceVersion)

	missing := newRenderedValuesUnderTest(t, &stubValuesFetcher{values: map[string][]byte{}}, cm)
	got, err = missing.GetRenderedValues(context.Background(), "demo", "sql")
	require.NoError(t, err)
	require.Contains(t, got[0].DefaultsError, "has no vendor/trino/values.yaml")
}

func TestRenderedValuesOfAnUnknownInstance(t *testing.T) {
	svc := newRenderedValuesUnderTest(t, &stubValuesFetcher{})
	_, err := svc.GetRenderedValues(context.Background(), "demo", "nope")
	require.True(t, apierrors.IsNotFound(err), "want NotFound, got %v", err)

	got, err := svc.GetRenderedValues(context.Background(), "demo", "sql")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
}
