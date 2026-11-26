package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocd"
	"github.com/okdp/okdp-control-plane-server/internal/gitops/flux"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

var (
	platformRef     = repository.ContextRef{Name: "platform", Namespace: "okdp-system"}
	controlPlaneRef = repository.ContextRef{Name: "control-plane", Namespace: "okdp-system"}
)

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newClients(objects ...runtime.Object) (clients, *dynamicfake.FakeDynamicClient, *fake.Clientset) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		crd.GetReleaseGVR():           "ReleaseList",
		crd.GetHelmReleaseGVR():       "HelmReleaseList",
		crd.GetConnectionGVR():        "ConnectionList",
		crd.GetClusterConnectionGVR(): "ClusterConnectionList",
		crd.GetSecretStoreGVR():       "SecretStoreList",
		crd.GetExternalSecretGVR():    "ExternalSecretList",
		{Group: "kubocd.kubotal.io", Version: "v1alpha1", Resource: "contexts"}: "ContextList",
		flux.GitRepositoryResource: "GitRepositoryList",
		flux.KustomizationResource: "KustomizationList",
	}, objects...)
	typed := fake.NewSimpleClientset()
	typed.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{
		{
			GroupVersion: crd.GetSecretStoreGVR().GroupVersion().String(),
			APIResources: []metav1.APIResource{
				{Name: "secretstores", Kind: crd.SecretStoreKind, Namespaced: true},
				{Name: "externalsecrets", Kind: crd.ExternalSecretKind, Namespaced: true},
			},
		},
		{
			GroupVersion: crd.GetConnectionGVR().GroupVersion().String(),
			APIResources: []metav1.APIResource{{Name: "connections", Kind: crd.ConnectionKind, Namespaced: true}},
		},
	}
	return clients{dynamic: dyn, typed: typed, discovery: typed.Discovery()}, dyn, typed
}

func syncObject(gvr schema.GroupVersionResource, kind, name string, labels map[string]any) *unstructured.Unstructured {
	meta := map[string]any{"name": name, "namespace": "okdp-system"}
	if labels != nil {
		meta["labels"] = labels
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(), "kind": kind, "metadata": meta,
	}}
}

func kubernetesOptions(cleanup string) stateOptions {
	cfg := kubocd.Config{Backend: "flux", Persistence: kubocd.PersistenceConfig{
		Mode: kubocd.PersistenceKubernetes, Git: kubocd.GitConfig{SyncNamespace: "okdp-system"},
	}, SyncCleanupName: cleanup}
	return stateOptions{engine: cfg, platform: platformRef, controlPlane: controlPlaneRef}
}

func exists(t *testing.T, dyn *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource, name string) bool {
	t.Helper()
	_, err := dyn.Tracker().Get(gvr, "okdp-system", name)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestKubernetesModeWritesToTheCluster(t *testing.T) {
	cl, _, _ := newClients()

	state, err := buildDesiredState(testCtx(t), cl, kubernetesOptions(""))

	require.NoError(t, err)
	assert.Equal(t, engine.PersistenceKubernetes, state.engine.Info.Persistence.Mode)
	assert.NotContains(t, typeName(state.projects), "kubocdgit")
	assert.NotContains(t, typeName(state.connections), "kubocdgit")
	assert.NotContains(t, typeName(state.secretStores), "kubocdgit")
	assert.NotContains(t, typeName(state.externalSecrets), "kubocdgit")
}

func typeName(v any) string { return fmt.Sprintf("%T", v) }

func orphaning(obj *unstructured.Unstructured, policy string) *unstructured.Unstructured {
	obj.Object["spec"] = map[string]any{"deletionPolicy": policy}
	return obj
}

func TestBackToKubernetesModeDeletesTheSyncObjectsTheChartRendered(t *testing.T) {
	name := "okdp-control-plane-server"
	owned := map[string]any{LabelManagedByControlPlane: name}
	cl, dyn, _ := newClients(
		orphaning(syncObject(flux.KustomizationResource, "Kustomization", name, owned), "Orphan"),
		syncObject(flux.GitRepositoryResource, "GitRepository", name, owned),
		syncObject(flux.KustomizationResource, "Kustomization", "okdp-system", nil),
	)

	_, err := buildDesiredState(testCtx(t), cl, kubernetesOptions(name))

	require.NoError(t, err)
	assert.False(t, exists(t, dyn, flux.KustomizationResource, name))
	assert.False(t, exists(t, dyn, flux.GitRepositoryResource, name))
	assert.True(t, exists(t, dyn, flux.KustomizationResource, "okdp-system"), "another Kustomization is never touched")

	var deleted []string
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "delete" {
			deleted = append(deleted, a.GetResource().Resource)
		}
	}
	assert.Equal(t, []string{"kustomizations", "gitrepositories"}, deleted, "the Kustomization goes first, its source after")
}

func TestTheCleanupLeavesSyncObjectsItDidNotRender(t *testing.T) {
	name := "okdp-control-plane-server"
	cl, dyn, _ := newClients(
		orphaning(syncObject(flux.KustomizationResource, "Kustomization", name, nil), "Orphan"),
		syncObject(flux.GitRepositoryResource, "GitRepository", name, map[string]any{LabelManagedByControlPlane: "another-release"}),
	)

	_, err := buildDesiredState(testCtx(t), cl, kubernetesOptions(name))
	require.NoError(t, err)
	assert.True(t, exists(t, dyn, flux.KustomizationResource, name), "no label: created by hand")
	assert.True(t, exists(t, dyn, flux.GitRepositoryResource, name), "rendered for another release")

	_, err = buildDesiredState(testCtx(t), cl, kubernetesOptions(""))
	require.NoError(t, err)
	for _, a := range dyn.Actions() {
		assert.NotEqual(t, "delete", a.GetVerb(), "no cleanup name, no deletion")
	}
}

func TestAKustomizationThatWouldPruneIsNeverDeleted(t *testing.T) {
	name := "okdp-control-plane-server"
	owned := map[string]any{LabelManagedByControlPlane: name}
	for _, policy := range []string{"", "MirrorPrune", "Delete"} {
		cl, dyn, _ := newClients(
			orphaning(syncObject(flux.KustomizationResource, "Kustomization", name, owned), policy),
			syncObject(flux.GitRepositoryResource, "GitRepository", name, owned),
		)

		_, err := buildDesiredState(testCtx(t), cl, kubernetesOptions(name))

		require.Error(t, err, "policy %q", policy)
		assert.Contains(t, err.Error(), "deletionPolicy")
		assert.True(t, exists(t, dyn, flux.KustomizationResource, name), "policy %q", policy)
		assert.True(t, exists(t, dyn, flux.GitRepositoryResource, name), "nothing deleted, policy %q", policy)
	}
}

func TestTheServerDoesNotStartWhileTheKustomizationMayStillApply(t *testing.T) {
	name := "okdp-control-plane-server"
	owned := map[string]any{LabelManagedByControlPlane: name}
	failures := map[string]func(*dynamicfake.FakeDynamicClient){
		"unreadable": func(dyn *dynamicfake.FakeDynamicClient) {
			dyn.PrependReactor("get", "kustomizations", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(flux.KustomizationResource.GroupResource(), name, errors.New("denied"))
			})
		},
		"not deletable": func(dyn *dynamicfake.FakeDynamicClient) {
			dyn.PrependReactor("delete", "kustomizations", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewInternalError(errors.New("etcd unavailable"))
			})
		},
		"never gone": func(dyn *dynamicfake.FakeDynamicClient) {
			dyn.PrependReactor("delete", "kustomizations", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, nil
			})
		},
	}
	kustomizationGoneWait, kustomizationGoneTick = 50*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { kustomizationGoneWait, kustomizationGoneTick = 30*time.Second, 200*time.Millisecond })
	for reason, fail := range failures {
		t.Run(reason, func(t *testing.T) {
			cl, dyn, _ := newClients(
				orphaning(syncObject(flux.KustomizationResource, "Kustomization", name, owned), "Orphan"),
				syncObject(flux.GitRepositoryResource, "GitRepository", name, owned),
			)
			fail(dyn)

			_, err := buildDesiredState(testCtx(t), cl, kubernetesOptions(name))

			require.Error(t, err)
			assert.Contains(t, err.Error(), "Kustomization okdp-system/"+name)
			assert.True(t, exists(t, dyn, flux.GitRepositoryResource, name), "the source stays while the Kustomization may apply")
		})
	}
}

func TestTheGitRepositoryCleanupIsBestEffort(t *testing.T) {
	name := "okdp-control-plane-server"
	owned := map[string]any{LabelManagedByControlPlane: name}
	cl, dyn, _ := newClients(syncObject(flux.GitRepositoryResource, "GitRepository", name, owned))
	dyn.PrependReactor("delete", "gitrepositories", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(flux.GitRepositoryResource.GroupResource(), name, errors.New("denied"))
	})

	_, err := buildDesiredState(testCtx(t), cl, kubernetesOptions(name))

	require.NoError(t, err, "no Kustomization applies it: a GitRepository left behind does not block the server")
}

func TestCleanupKustomizationRetriesConflictAndWaitsWhenAlreadyDeleting(t *testing.T) {
	name := "okdp-control-plane-server"
	owned := map[string]any{LabelManagedByControlPlane: name}

	t.Run("retries conflict while still Orphan", func(t *testing.T) {
		ks := orphaning(syncObject(flux.KustomizationResource, "Kustomization", name, owned), "Orphan")
		ks.SetResourceVersion("1")
		cl, dyn, _ := newClients(ks)
		conflicts := 0
		dyn.PrependReactor("delete", "kustomizations", func(k8stesting.Action) (bool, runtime.Object, error) {
			if conflicts == 0 {
				conflicts++
				return true, nil, apierrors.NewConflict(flux.KustomizationResource.GroupResource(), name, errors.New("rv changed"))
			}
			return false, nil, nil
		})

		_, err := buildDesiredState(testCtx(t), cl, kubernetesOptions(name))
		require.NoError(t, err)
		assert.Equal(t, 1, conflicts)
		assert.False(t, exists(t, dyn, flux.KustomizationResource, name))
	})

	t.Run("stops retrying if policy changed away from Orphan", func(t *testing.T) {
		ks := orphaning(syncObject(flux.KustomizationResource, "Kustomization", name, owned), "Orphan")
		cl, dyn, _ := newClients(ks)
		dyn.PrependReactor("delete", "kustomizations", func(k8stesting.Action) (bool, runtime.Object, error) {
			cur, err := dyn.Tracker().Get(flux.KustomizationResource, "okdp-system", name)
			require.NoError(t, err)
			u := cur.(*unstructured.Unstructured).DeepCopy()
			require.NoError(t, unstructured.SetNestedField(u.Object, "Delete", "spec", "deletionPolicy"))
			require.NoError(t, dyn.Tracker().Update(flux.KustomizationResource, u, "okdp-system"))
			return true, nil, apierrors.NewConflict(flux.KustomizationResource.GroupResource(), name, errors.New("policy changed"))
		})

		_, err := buildDesiredState(testCtx(t), cl, kubernetesOptions(name))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "deletionPolicy")
		assert.True(t, exists(t, dyn, flux.KustomizationResource, name))
	})

	t.Run("already terminating with Orphan skips Delete and waits for disappearance", func(t *testing.T) {
		ks := orphaning(syncObject(flux.KustomizationResource, "Kustomization", name, owned), "Orphan")
		now := metav1.Now()
		ks.SetDeletionTimestamp(&now)
		cl, dyn, _ := newClients(ks)
		kustomizationGoneWait, kustomizationGoneTick = time.Second, 5*time.Millisecond
		t.Cleanup(func() { kustomizationGoneWait, kustomizationGoneTick = 30*time.Second, 200*time.Millisecond })

		gets := 0
		dyn.PrependReactor("get", "kustomizations", func(k8stesting.Action) (bool, runtime.Object, error) {
			gets++
			if gets >= 3 {
				return true, nil, apierrors.NewNotFound(flux.KustomizationResource.GroupResource(), name)
			}
			return false, nil, nil
		})

		_, err := buildDesiredState(testCtx(t), cl, kubernetesOptions(name))
		require.NoError(t, err)
		for _, a := range dyn.Actions() {
			assert.NotEqual(t, "delete", a.GetVerb(), "no second Delete call when DeletionTimestamp is already set")
		}
	})
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	bin, err := exec.LookPath("git")
	require.NoError(t, err, "these tests need the git binary")
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

func gitOptions(t *testing.T, bare string, initialize bool) stateOptions {
	cfg := kubocd.Config{Backend: "flux", ReleaseInterval: "30m0s", ReleaseTimeout: "10m0s", Persistence: kubocd.PersistenceConfig{
		Mode: kubocd.PersistenceGit,
		Git: kubocd.GitConfig{
			URL: bare, Branch: "main", Path: "clusters/dev", Sync: kubocd.SyncNone,
			Initialize: initialize, CredentialsDir: filepath.Join(t.TempDir(), "absent"), SyncNamespace: "okdp-system",
		},
	}}
	return stateOptions{engine: cfg, platform: platformRef, controlPlane: controlPlaneRef}
}

func newBare(t *testing.T) string {
	bare := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, ".", "init", "--quiet", "--bare", "--initial-branch=main", bare)
	return bare
}

func TestGitModeInitializesTheBranchAndWritesNothingToTheCluster(t *testing.T) {
	bare := newBare(t)
	cl, dyn, typed := newClients()
	ctx := testCtx(t)

	state, err := buildDesiredState(ctx, cl, gitOptions(t, bare, true))
	require.NoError(t, err)
	assert.Equal(t, engine.PersistenceGit, state.engine.Info.Persistence.Mode)
	assert.Contains(t, runGit(t, bare, "ls-tree", "-r", "--name-only", "main"), "clusters/dev/contexts/control-plane.yaml")

	running, stop := context.WithCancel(ctx)
	state.start(running)
	defer stop()

	require.NoError(t, state.projects.Create(ctx, &models.Project{Name: "demo", Description: "Demo"}))
	require.NoError(t, state.connections.Create(ctx, "demo", &crd.Connection{
		TypeMeta:   metav1.TypeMeta{APIVersion: crd.ConnectionAPIVersion, Kind: crd.ConnectionKind},
		ObjectMeta: metav1.ObjectMeta{Name: "pg"},
		Spec:       crd.ConnectionSpec{Contract: "database-server"},
	}))
	require.NoError(t, state.secretStores.Create(ctx, "demo", &crd.ESOSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "vault"},
		Spec: crd.ESOSecretStoreSpec{Provider: crd.ESOProvider{Vault: &crd.ESOVaultProvider{
			Server: "https://vault.example.com", Path: "secret", Version: "v2",
			Auth: crd.ESOVaultAuth{Kubernetes: &crd.ESOKubernetesAuth{MountPath: "kubernetes", Role: "reader"}},
		}}},
	}))
	require.NoError(t, state.externalSecrets.Create(ctx, "demo", &crd.ESOExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "db"},
		Spec: crd.ESOExternalSecretSpec{
			SecretStoreRef: crd.ESOSecretStoreRef{Name: "vault", Kind: "SecretStore"},
			Target:         crd.ESOExternalSecretTarget{Name: "db", CreationPolicy: "Owner"},
		},
	}))
	_, err = state.engine.DesiredState.CreateInstance(ctx, engine.Instance{
		Ref: engine.InstanceRef{Project: "demo", Name: "pg"}, Service: "postgres", Version: "1.0.0",
	}, engine.WriteOptions{})
	_ = err // the catalog is empty: the point is what reached the cluster

	tree := runGit(t, bare, "ls-tree", "-r", "--name-only", "main")
	for _, f := range []string{
		"clusters/dev/projects/demo/namespace.yaml",
		"clusters/dev/projects/demo/connections/pg.yaml",
		"clusters/dev/projects/demo/secret-stores/vault.yaml",
		"clusters/dev/projects/demo/external-secrets/db.yaml",
	} {
		assert.Contains(t, tree, f)
	}
	desired := map[string]bool{
		"namespaces": true, "contexts": true, "connections": true, "releases": true,
		"secretstores": true, "externalsecrets": true, "persistentvolumeclaims": true,
	}
	for _, a := range append(dyn.Actions(), typed.Actions()...) {
		switch a.GetVerb() {
		case "create", "update", "patch", "delete":
			assert.False(t, desired[a.GetResource().Resource], "git mode wrote %s %s to the cluster", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

func TestGitModeWithoutInitializeLeavesTheBranchAlone(t *testing.T) {
	bare := newBare(t)
	cl, _, _ := newClients()

	_, err := buildDesiredState(testCtx(t), cl, gitOptions(t, bare, false))

	require.NoError(t, err)
	assert.Empty(t, runGit(t, bare, "for-each-ref", "refs/heads/main"), "nothing committed")
}

func TestGitModeRefusesSharedPlatformAndControlPlaneContext(t *testing.T) {
	bare := newBare(t)
	cl, _, _ := newClients()
	opts := gitOptions(t, bare, false)
	opts.controlPlane = opts.platform

	_, err := buildDesiredState(testCtx(t), cl, opts)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be distinct from the read-only platform Context")
}

func TestGitModeValidatesCredentialsAtStartupEvenWithoutInitialize(t *testing.T) {
	cl, _, _ := newClients()
	opts := gitOptions(t, "ssh://git@git.example.com/org/repo.git", false)

	_, err := buildDesiredState(testCtx(t), cl, opts)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "credentials")
}

func TestGitModeWithFluxStopsOnSyncObjectsThatPointElsewhere(t *testing.T) {
	bare := newBare(t)
	opts := gitOptions(t, bare, true)
	opts.engine.Persistence.Git.URL = "https://forge.example/okdp/deployments.git"
	opts.engine.Persistence.Git.Sync = kubocd.SyncFlux
	opts.engine.Persistence.Git.GitRepositoryName = "okdp-control-plane-server"
	opts.engine.Persistence.Git.KustomizationName = "okdp-control-plane-server"
	repo := syncObject(flux.GitRepositoryResource, "GitRepository", "okdp-control-plane-server", nil)
	repo.Object["spec"] = map[string]any{"url": "https://forge.example/okdp/other.git", "ref": map[string]any{"branch": "main"}}
	ks := syncObject(flux.KustomizationResource, "Kustomization", "okdp-control-plane-server", nil)
	ks.Object["spec"] = map[string]any{"path": "./clusters/dev", "sourceRef": map[string]any{"kind": "GitRepository", "name": "okdp-control-plane-server"}}
	cl, _, _ := newClients(repo, ks)

	started := time.Now()
	_, err := buildDesiredState(testCtx(t), cl, opts)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "do not match the server repository")
	assert.Less(t, time.Since(started), syncValidateWait, "a mismatch is not waited for")
}

func fluxOptions(t *testing.T) stateOptions {
	opts := gitOptions(t, "https://forge.example/okdp/deployments.git", false)
	opts.engine.Persistence.Git.Sync = kubocd.SyncFlux
	opts.engine.Persistence.Git.GitRepositoryName = "okdp-control-plane-server"
	opts.engine.Persistence.Git.KustomizationName = "okdp-control-plane-server"
	return opts
}

func TestGitModeWithFluxStartsOnMatchingSyncObjects(t *testing.T) {
	repo := syncObject(flux.GitRepositoryResource, "GitRepository", "okdp-control-plane-server", nil)
	repo.Object["spec"] = map[string]any{"url": "https://forge.example/okdp/deployments.git", "ref": map[string]any{"branch": "main"}}
	ks := syncObject(flux.KustomizationResource, "Kustomization", "okdp-control-plane-server", nil)
	ks.Object["spec"] = map[string]any{
		"path": "./clusters/dev", "prune": true, "serviceAccountName": "okdp-control-plane-server-gitops",
		"sourceRef": map[string]any{"kind": "GitRepository", "name": "okdp-control-plane-server"},
	}
	cl, dyn, _ := newClients(repo, ks)
	ctx := testCtx(t)

	state, err := buildDesiredState(ctx, cl, fluxOptions(t))
	require.NoError(t, err)
	assert.Equal(t, engine.PersistenceGit, state.engine.Info.Persistence.Mode)

	running, stop := context.WithCancel(ctx)
	defer stop()
	state.start(running)
	require.Eventually(t, func() bool {
		for _, a := range dyn.Actions() {
			if a.GetVerb() == "watch" && a.GetResource() == flux.KustomizationResource {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "the adapter follows the Kustomization")
}

type validatorFunc func(ctx context.Context) error

func (f validatorFunc) Validate(ctx context.Context) error { return f(ctx) }

func TestValidateSyncWaitsOnlyForMissingObjects(t *testing.T) {
	missing := apierrors.NewNotFound(flux.GitRepositoryResource.GroupResource(), "okdp")
	calls := 0
	err := validateSync(context.Background(), validatorFunc(func(context.Context) error {
		calls++
		if calls < 3 {
			return missing
		}
		return nil
	}), time.Second, time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, 3, calls, "created by the same helm install: waited for")

	calls = 0
	mismatch := errors.New("flux: the sync objects do not match the server repository")
	err = validateSync(context.Background(), validatorFunc(func(context.Context) error {
		calls++
		return mismatch
	}), time.Second, time.Millisecond)
	assert.ErrorIs(t, err, mismatch)
	assert.Equal(t, 1, calls)

	err = validateSync(context.Background(), validatorFunc(func(context.Context) error { return missing }), 20*time.Millisecond, time.Millisecond)
	assert.True(t, apierrors.IsNotFound(err), "still missing after the wait: %v", err)
}

func TestServeShutsDownOnTheSignalEvenWithAStreamOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	streaming := make(chan struct{})
	restStarted := make(chan struct{})
	releaseREST := make(chan struct{})
	restCtxErr := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		sctx, scancel := context.WithCancel(r.Context())
		defer scancel()
		stop := context.AfterFunc(ctx, scancel)
		defer stop()
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(streaming)
		<-sctx.Done()
	})
	mux.HandleFunc("/write", func(w http.ResponseWriter, r *http.Request) {
		close(restStarted)
		<-releaseREST
		restCtxErr <- r.Context().Err()
		w.WriteHeader(http.StatusOK)
	})
	// No BaseContext: only SSE routes listen to ctx, while in-flight REST
	// writes finish cleanly during server.Shutdown.
	server := &http.Server{Handler: mux}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, server, ln, 5*time.Second) }()

	streamResp, err := http.Get("http://" + ln.Addr().String() + "/stream")
	require.NoError(t, err)
	defer streamResp.Body.Close()
	<-streaming

	restDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/write")
		if err == nil {
			_ = resp.Body.Close()
		}
		restDone <- err
	}()
	<-restStarted

	started := time.Now()
	cancel()
	// Give shutdown a moment to cancel the stream while the REST write is still running.
	time.Sleep(50 * time.Millisecond)
	close(releaseREST)

	require.NoError(t, <-restDone)
	assert.NoError(t, <-restCtxErr, "in-flight REST write context must NOT be cancelled on SIGTERM")

	select {
	case err := <-done:
		require.NoError(t, err)
		assert.Less(t, time.Since(started), 5*time.Second, "the stream ended with the signal and the REST write drained cleanly")
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after the signal")
	}
}

func TestServeReportsAListenerThatFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, ln.Close())

	err = serve(context.Background(), &http.Server{}, ln, time.Second)
	assert.Error(t, err)
}
