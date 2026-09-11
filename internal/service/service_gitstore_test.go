package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
)

func init() {
	// Serve file:// in process: the tests need no git binary.
	client.InstallProtocol("file", server.DefaultServer)
}

// newGitStore returns two GitStores on the same fresh bare repository: the
// server's and another writer's.
func newGitStore(t *testing.T) (*gitops.GitStore, *gitops.GitStore) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	_, err := git.PlainInit(dir, true)
	require.NoError(t, err)
	open := func() *gitops.GitStore {
		s, err := gitops.NewGitStore(gitops.GitOptions{URL: "file://" + dir, Branch: "main", RefreshInterval: time.Nanosecond})
		require.NoError(t, err)
		return s
	}
	return open(), open()
}

// catalogSchema resolves the schema the way the server does: through the
// catalog, which is read from the deployments repository. onRead runs after
// each read, before the schema is returned.
type catalogSchema struct {
	PackageSchemaService
	deployments *gitops.Deployments
	onRead      func()
}

func (s catalogSchema) GetParameterSchema(ctx context.Context, _, _ string) (map[string]any, error) {
	if _, err := s.deployments.ReadCatalog(ctx); err != nil {
		return nil, err
	}
	if s.onRead != nil {
		s.onRead()
	}
	return trinoSchema(), nil
}

// run fails the test when fn does not return in time: a deadlock on the
// store's lock must fail the test, not hang it.
func run(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out: the write deadlocked on the deployments repository")
	}
}

func seedTrino(t *testing.T, store gitops.Store) *gitops.Deployments {
	t.Helper()
	d := gitops.NewDeployments(store, nil)
	_, err := d.Store.Update(context.Background(), gitops.Commit{Subject: "seed"}, func(tx gitops.Tx) error {
		return tx.WriteFile(gitops.CatalogPath, []byte("categories: []\n"))
	})
	require.NoError(t, err)
	_, err = d.CreateInstance(context.Background(), auth.ActorFrom(aliceContext()), gitops.InstanceState{
		Instance: gitops.Instance{Name: "trino", Project: "demo", Service: "trino",
			Chart: "oci://quay.io/okdp/platform-charts/trino", Version: "480.0.0-p21", Connections: []string{}},
		Values: map[string]any{"workers": 2},
	})
	require.NoError(t, err)
	return d
}

// Regression: validating a parameters PATCH reads the catalog from Git. Run
// inside the write callback, that read waited for the lock the write held,
// forever, and every later Git read with it. The memory store hides this; a
// real GitStore does not.
func TestUpdateParametersDoesNotDeadlockTheGitStore(t *testing.T) {
	store, _ := newGitStore(t)
	d := seedTrino(t, store)
	svc := NewDefaultServiceService(ServiceDeps{
		Deployments:   d,
		PlatformRepo:  stubPlatform{},
		SchemaService: catalogSchema{deployments: d},
	})

	run(t, func() {
		_, err := svc.UpdateServiceParameters(aliceContext(), "demo", "trino", models.ServiceUpdateRequest{Parameters: map[string]any{"workers": float64(3)}})
		assert.NoError(t, err)
		_, err = svc.UpdateServiceParameters(aliceContext(), "demo", "trino", models.ServiceUpdateRequest{Parameters: map[string]any{"workers": float64(0)}})
		assert.True(t, IsValidationError(err), "got %v", err)
	})

	st, err := d.GetInstance(context.Background(), "demo", "trino")
	require.NoError(t, err)
	assert.EqualValues(t, 3, st.Values["workers"], "the valid patch is committed, the invalid one is not")
}

// A declaration changed by another writer between the validation and the
// write is validated again: the committed values are the patch applied to
// what the other writer left, never the stale validation.
func TestUpdateParametersRevalidatesWhenTheInstanceChanged(t *testing.T) {
	store, other := newGitStore(t)
	d := seedTrino(t, store)
	otherDeployments := gitops.NewDeployments(other, nil)

	reads := 0
	schema := catalogSchema{deployments: d, onRead: func() {
		reads++
		if reads != 1 {
			return
		}
		_, _, err := otherDeployments.UpdateInstance(context.Background(), auth.ActorFrom(aliceContext()), "demo", "trino", func(st *gitops.InstanceState) error {
			st.Values["catalogs"] = []any{map[string]any{"name": "lake"}}
			return nil
		})
		assert.NoError(t, err)
	}}
	svc := NewDefaultServiceService(ServiceDeps{Deployments: d, PlatformRepo: stubPlatform{}, SchemaService: schema})

	run(t, func() {
		_, err := svc.UpdateServiceParameters(aliceContext(), "demo", "trino", models.ServiceUpdateRequest{Parameters: map[string]any{"workers": float64(4)}})
		assert.NoError(t, err)
	})

	assert.Equal(t, 2, reads, "validated once more on the changed declaration")
	st, err := d.GetInstance(context.Background(), "demo", "trino")
	require.NoError(t, err)
	assert.EqualValues(t, 4, st.Values["workers"])
	assert.Contains(t, st.Values, "catalogs", "the other writer's change is kept")
}
