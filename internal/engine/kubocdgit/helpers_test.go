package kubocdgit_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocd"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocdgit"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	gitopsfake "github.com/okdp/okdp-control-plane-server/internal/gitops/fake"
	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

// The tests run against throwaway bare repositories on disk: go-git's file
// transport runs git-upload-pack and git-receive-pack.

const storePath = "clusters/dev"

func gitEnv(t *testing.T) []string {
	home := t.TempDir()
	return append(os.Environ(),
		"HOME="+home, "XDG_CONFIG_HOME="+home, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Direct Pusher", "GIT_AUTHOR_EMAIL=direct@example.com",
		"GIT_COMMITTER_NAME=Direct Pusher", "GIT_COMMITTER_EMAIL=direct@example.com",
	)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	bin, err := exec.LookPath("git")
	require.NoError(t, err, "these tests need the git binary")
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

func newBare(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, ".", "init", "--quiet", "--bare", "--initial-branch=main", dir)
	return dir
}

// pushDirect commits files (paths from the repository root) the way a user
// with a clone would. A nil content deletes the file.
func pushDirect(t *testing.T, bare string, files map[string][]byte, message string) string {
	t.Helper()
	work := t.TempDir()
	if runGit(t, bare, "for-each-ref", "refs/heads/main") == "" {
		runGit(t, work, "init", "--quiet", "--initial-branch=main")
		runGit(t, work, "remote", "add", "origin", bare)
	} else {
		runGit(t, work, "clone", "--quiet", "--branch", "main", bare, ".")
	}
	for p, content := range files {
		if content == nil {
			runGit(t, work, "rm", "--quiet", p)
			continue
		}
		full := filepath.Join(work, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, content, 0o644))
		runGit(t, work, "add", p)
	}
	runGit(t, work, "commit", "--quiet", "-m", message)
	runGit(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	return runGit(t, work, "rev-parse", "HEAD")
}

func showFile(t *testing.T, bare, p string) string {
	return runGit(t, bare, "show", "main:"+storePath+"/"+p)
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newStore(t *testing.T, bare string) *gitstore.Store {
	t.Helper()
	s, err := gitstore.New(gitstore.Config{
		Repository: gitops.Repository{URL: bare, Branch: "main", Path: storePath},
		Committer:  gitstore.Author{Name: "OKDP Control Plane", Email: "control-plane@okdp.example"},
	})
	require.NoError(t, err)
	return s
}

// newCluster is a fake API server that keeps metadata.generation the way a
// real one does for a custom resource with a status subresource.
func newCluster() *dynamicfake.FakeDynamicClient {
	cluster := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		crd.GetReleaseGVR():           "ReleaseList",
		crd.GetHelmReleaseGVR():       "HelmReleaseList",
		crd.GetConnectionGVR():        "ConnectionList",
		crd.GetClusterConnectionGVR(): "ClusterConnectionList",
		{Group: "kubocd.kubotal.io", Version: "v1alpha1", Resource: "contexts"}: "ContextList",
		crd.GetSecretStoreGVR():    "SecretStoreList",
		crd.GetExternalSecretGVR(): "ExternalSecretList",
	})
	gvr := crd.GetReleaseGVR()
	cluster.PrependReactor("create", "releases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured).SetGeneration(1)
		return false, nil, nil
	})
	cluster.PrependReactor("update", "releases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		stored, err := cluster.Tracker().Get(gvr, obj.GetNamespace(), obj.GetName())
		if err != nil {
			return false, nil, nil
		}
		previous := stored.(*unstructured.Unstructured)
		obj.SetGeneration(previous.GetGeneration())
		if !reflect.DeepEqual(previous.Object["spec"], obj.Object["spec"]) {
			obj.SetGeneration(previous.GetGeneration() + 1)
		}
		return false, nil, nil
	})
	return cluster
}

// platformStub is the platform Context: what PlatformConfig reads. It holds
// no catalog of its own in these tests unless set.
type platformStub struct {
	repository.ContextRepository
	services []models.PlatformService
}

func (p *platformStub) Invalidate() {}
func (p *platformStub) GetIngressSuffix(context.Context) (string, error) {
	return "platform.example", nil
}
func (p *platformStub) GetPlatformServices(context.Context) ([]models.PlatformService, error) {
	return p.services, nil
}
func (p *platformStub) GetMenuCategories(context.Context) ([]models.MenuCategory, error) {
	return []models.MenuCategory{{Key: "Data Science", Label: "Data Science", Order: 1}}, nil
}
func (p *platformStub) GetPackageRepository(context.Context) (string, error) {
	return "quay.io/okdp/packages", nil
}

func kubernetesCatalog(context.Context) (map[string]any, bool, error) {
	return map[string]any{
		"defaultRepository": "quay.io/okdp/packages",
		"categories": []any{map[string]any{
			"title": "Data Science",
			"services": []any{
				map[string]any{"name": "jupyterhub", "default": "1.0.0", "versions": []any{"1.0.0", "1.1.0", "2.0.0"}},
				map[string]any{"name": "trino", "default": "1.0.0", "versions": []any{"1.0.0"}},
			},
		}},
	}, true, nil
}

// env is one git-backed engine with its remote, cluster and sync tool.
type env struct {
	t       *testing.T
	bare    string
	store   *gitstore.Store
	cluster *dynamicfake.FakeDynamicClient
	sync    *gitopsfake.Adapter
	adapter *kubocdgit.Adapter
	engine  engine.Engine
}

func newEnv(t *testing.T) *env {
	t.Helper()
	bare := newBare(t)
	store := newStore(t, bare)
	_, err := store.Bootstrap(testCtx(t))
	require.NoError(t, err)
	e := &env{t: t, bare: bare, store: store, cluster: newCluster(), sync: gitopsfake.New()}
	e.adapter = kubocdgit.New(kubocdgit.Deps{
		Store:               store,
		Sync:                e.sync,
		Platform:            &platformStub{},
		Cluster:             e.cluster,
		Config:              kubocd.Config{ReleaseInterval: "30m0s", ReleaseTimeout: "10m0s"},
		ControlPlaneContext: repository.ContextRef{Name: "control-plane", Namespace: "okdp-system"},
		Catalog:             kubernetesCatalog,
		CacheTTL:            time.Hour,
		PollInterval:        time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.adapter.Start(ctx)
	_, err = e.adapter.BootstrapControlPlaneContext(testCtx(t))
	require.NoError(t, err)
	e.engine = e.adapter.Engine()
	return e
}

func (e *env) head() string {
	return runGit(e.t, e.bare, "rev-parse", "refs/heads/main")
}

// converge plays Flux and the KuboCD controller for one Release: the
// cluster gets the file as committed at the head of the branch, reconciled
// and Ready, and the sync tool reports the head applied.
func (e *env) converge(t *testing.T, ref engine.InstanceRef) {
	t.Helper()
	e.apply(t, ref.Project, ref.Project+"-"+ref.Name, true)
	e.applied(e.head())
}

// fetched reports rev fetched by the sync tool, not applied yet.
func (e *env) fetched(rev string) {
	e.setSync(gitops.SyncState{Phase: gitops.SyncProgressing, FetchedRevision: rev})
}

func (e *env) applied(rev string) {
	e.setSync(gitops.SyncState{Phase: gitops.SyncReady, AppliedRevision: rev, FetchedRevision: rev})
}

// setSync reports st and waits until the adapter took it in.
func (e *env) setSync(st gitops.SyncState) {
	e.t.Helper()
	st.ObservedAt = time.Now()
	e.sync.SetState(st)
	require.Eventually(e.t, func() bool {
		got := e.adapter.SyncState()
		return got.ObservedAt.Equal(st.ObservedAt)
	}, eventually, tick)
}

// apply makes the cluster hold the Release name as the head of the branch
// declares it, or removes it when the file is gone. ready marks it
// reconciled for its generation.
func (e *env) apply(t *testing.T, project, name string, ready bool) {
	t.Helper()
	ctx := testCtx(t)
	releases := e.cluster.Resource(crd.GetReleaseGVR()).Namespace(project)
	snap, err := newStore(t, e.bare).Latest(ctx)
	require.NoError(t, err)
	p, err := gitstore.ReleaseFile(project, name)
	require.NoError(t, err)
	data, err := snap.ReadFile(p)
	if errors.Is(err, gitstore.ErrNotFound) {
		err = releases.Delete(ctx, name, metav1.DeleteOptions{})
		if !apierrors.IsNotFound(err) {
			require.NoError(t, err)
		}
		return
	}
	require.NoError(t, err)
	var obj map[string]any
	require.NoError(t, sigsyaml.Unmarshal(data, &obj))
	desired := &unstructured.Unstructured{Object: obj}
	labels := desired.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["kustomize.toolkit.fluxcd.io/name"] = "okdp"
	desired.SetLabels(labels)

	current, err := releases.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		current, err = releases.Create(ctx, desired, metav1.CreateOptions{})
	} else if err == nil {
		desired.SetResourceVersion(current.GetResourceVersion())
		desired.Object["status"] = current.Object["status"]
		current, err = releases.Update(ctx, desired, metav1.UpdateOptions{})
	}
	require.NoError(t, err)
	if !ready {
		return
	}
	current.Object["status"] = map[string]any{
		"phase":              "READY",
		"observedGeneration": current.GetGeneration(),
		"conditions":         []any{map[string]any{"type": "Ready", "status": "True", "reason": "Succeeded"}},
	}
	_, err = releases.Update(ctx, current, metav1.UpdateOptions{})
	require.NoError(t, err)
}

const (
	eventually = 5 * time.Second
	tick       = 20 * time.Millisecond
)
