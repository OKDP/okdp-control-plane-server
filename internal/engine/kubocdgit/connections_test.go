package kubocdgit_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocdgit"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

type connectionsEnv struct {
	*env
	client *fake.Clientset
	repo   *kubocdgit.ConnectionRepository
	stores *kubocdgit.SecretStoreRepository
	svc    *service.DefaultConnectionService
}

// reap runs the reaper the server runs: Connections and SecretStores.
func (c *connectionsEnv) reap(ctx context.Context) error {
	return kubocdgit.NewCredentialsReaper(c.adapter, c.client, c.repo, c.stores).Reap(ctx)
}

func newConnections(t *testing.T) *connectionsEnv {
	t.Helper()
	e := newEnv(t)
	client := fake.NewSimpleClientset()
	client.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{
		GroupVersion: crd.GetConnectionGVR().GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: "connections", Kind: crd.ConnectionKind, Namespaced: true}},
	}}
	cluster := repository.NewConnectionRepository(e.cluster, client, client.Discovery())
	repo := kubocdgit.NewConnectionRepository(e.adapter, cluster, client)
	catalog, err := service.NewEmbeddedContractCatalog()
	require.NoError(t, err)
	svc := service.NewDefaultConnectionService(repo, repository.NewServiceRepository(e.cluster), catalog)
	stores := kubocdgit.NewSecretStoreRepository(e.adapter, repository.NewSecretStoreRepository(e.cluster, client.Discovery()), client)
	return &connectionsEnv{env: e, client: client, repo: repo, stores: stores, svc: svc}
}

func warehouse() models.ConnectionRequest {
	return models.ConnectionRequest{
		Name:        "warehouse",
		Type:        "database-server",
		Description: "Corporate warehouse",
		Values: map[string]any{
			"engine": "postgresql", "driver": "org.postgresql.Driver", "host": "db.example.com",
			"port": float64(5432), "dbName": "analytics", "username": "reader", "password": "s3cret", "sslMode": "require",
		},
	}
}

// applyConnection plays the sync tool for one Connection: the cluster gets
// the file at the head of the branch, or loses it when the file is gone.
func (c *connectionsEnv) applyConnection(t *testing.T, namespace, name string) {
	t.Helper()
	ctx := testCtx(t)
	resource := c.cluster.Resource(crd.GetConnectionGVR()).Namespace(namespace)
	snap, err := newStore(t, c.bare).Latest(ctx)
	require.NoError(t, err)
	data, err := snap.ReadFile("projects/" + namespace + "/connections/" + name + ".yaml")
	if err != nil {
		err = resource.Delete(ctx, name, metav1.DeleteOptions{})
		if !apierrors.IsNotFound(err) {
			require.NoError(t, err)
		}
		return
	}
	obj := map[string]any{}
	require.NoError(t, sigsyaml.Unmarshal(data, &obj))
	desired := &unstructured.Unstructured{Object: obj}
	if current, err := resource.Get(ctx, name, metav1.GetOptions{}); err == nil {
		desired.SetResourceVersion(current.GetResourceVersion())
		_, err = resource.Update(ctx, desired, metav1.UpdateOptions{})
		require.NoError(t, err)
		return
	}
	_, err = resource.Create(ctx, desired, metav1.CreateOptions{})
	require.NoError(t, err)
}

func (c *connectionsEnv) secretExists(t *testing.T, namespace, name string) bool {
	_, err := c.client.CoreV1().Secrets(namespace).Get(testCtx(t), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestAConnectionIsCommittedAndItsCredentialsStayInTheCluster(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)

	created, err := c.svc.Create(ctx, "demo", warehouse())
	require.NoError(t, err)
	assert.Equal(t, "warehouse", created.Name)

	content := showFile(t, c.bare, "projects/demo/connections/warehouse.yaml")
	assert.Contains(t, content, "kind: Connection")
	assert.Contains(t, content, "contract: database-server")
	assert.Contains(t, content, "secretRef: warehouse-credentials")
	assert.NotContains(t, content, "s3cret", "no credential in git")
	assert.True(t, c.secretExists(t, "demo", "warehouse-credentials"))

	listed, err := c.svc.List(ctx, "demo")
	require.NoError(t, err)
	require.Len(t, listed, 1, "a committed Connection is listed before the sync tool applies it")

	_, err = c.svc.Create(ctx, "demo", warehouse())
	assert.True(t, apierrors.IsAlreadyExists(err), "%v", err)
}

func TestAnUpdateKeepsTheConnectionFieldsItDoesNotOwn(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	_, err := c.svc.Create(ctx, "demo", warehouse())
	require.NoError(t, err)
	content := showFile(t, c.bare, "projects/demo/connections/warehouse.yaml")
	pushDirect(t, c.bare, map[string][]byte{storePath + "/projects/demo/connections/warehouse.yaml": []byte(content + "\n# reviewed by the data team\nextra:\n  ticket: DATA-12\n")}, "hand edit")

	req := warehouse()
	req.Description = "Moved"
	delete(req.Values, "username")
	delete(req.Values, "password")
	_, err = c.svc.Update(ctx, "demo", "warehouse", req)
	require.NoError(t, err)

	content = showFile(t, c.bare, "projects/demo/connections/warehouse.yaml")
	assert.Contains(t, content, "description: Moved")
	assert.Contains(t, content, "ticket: DATA-12")
	assert.Contains(t, content, "# reviewed by the data team")
	assert.Contains(t, content, "secretRef: warehouse-credentials", "unchanged credentials keep their reference")
}

func TestTheSecretOfADeletedConnectionOutlivesItUntilTheClusterDropsIt(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	_, err := c.svc.Create(ctx, "demo", warehouse())
	require.NoError(t, err)
	c.applyConnection(t, "demo", "warehouse")

	require.NoError(t, c.svc.Delete(ctx, "demo", "warehouse"))
	_, err = c.client.CoreV1().Secrets("demo").Get(ctx, "warehouse-credentials", metav1.GetOptions{})
	require.NoError(t, err, "the Secret is not deleted when the deletion is committed")
	assert.NotContains(t, runGit(t, c.bare, "ls-tree", "-r", "--name-only", "main"), "warehouse.yaml")

	// The commit is not applied: the cluster still holds the Connection.
	require.NoError(t, c.reap(ctx))
	assert.True(t, c.secretExists(t, "demo", "warehouse-credentials"), "the running consumers keep their credentials")

	c.applyConnection(t, "demo", "warehouse")
	require.NoError(t, c.reap(ctx))
	assert.False(t, c.secretExists(t, "demo", "warehouse-credentials"), "deleted once the Connection is gone from git and from the cluster")
}

func TestASecretAConnectionMovedAwayFromStaysUntilTheChangeIsApplied(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	_, err := c.svc.Create(ctx, "demo", warehouse())
	require.NoError(t, err)
	c.applyConnection(t, "demo", "warehouse")
	_, err = c.client.CoreV1().Secrets("demo").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-projected", Namespace: "demo"},
		Data:       map[string][]byte{"username": []byte("u"), "password": []byte("x")},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	req := warehouse()
	req.ExistingSecret = "vault-projected"
	_, err = c.svc.Update(ctx, "demo", "warehouse", req)
	require.NoError(t, err)

	require.NoError(t, c.reap(ctx))
	assert.True(t, c.secretExists(t, "demo", "warehouse-credentials"), "the cluster Connection still points at it")

	c.applyConnection(t, "demo", "warehouse")
	require.NoError(t, c.reap(ctx))
	assert.False(t, c.secretExists(t, "demo", "warehouse-credentials"))
	assert.True(t, c.secretExists(t, "demo", "vault-projected"), "a Secret the control plane does not own is never touched")
}

func TestAMarkedSecretWrittenAgainIsNotReaped(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	_, err := c.svc.Create(ctx, "demo", warehouse())
	require.NoError(t, err)
	require.NoError(t, c.svc.Delete(ctx, "demo", "warehouse"))

	// Created again before the reaper ran: the new Secret drops the mark.
	_, err = c.svc.Create(ctx, "demo", warehouse())
	require.NoError(t, err)
	require.NoError(t, c.reap(ctx))
	assert.True(t, c.secretExists(t, "demo", "warehouse-credentials"))
}

func TestAConnectionOnlyInTheClusterIsListedButNotChangedFromGit(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	published := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": crd.ConnectionAPIVersion, "kind": crd.ConnectionKind,
		"metadata": map[string]any{"name": "jh-hub", "namespace": "demo"},
		"spec":     map[string]any{"contract": "http-endpoint", "outputName": "hub"},
		"status":   map[string]any{"parent": "demo-jh"},
	}}
	_, err := c.cluster.Resource(crd.GetConnectionGVR()).Namespace("demo").Create(ctx, published, metav1.CreateOptions{})
	require.NoError(t, err)

	listed, err := c.repo.List(ctx, "demo")
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.True(t, listed[0].IsManaged())

	err = c.svc.Delete(ctx, "demo", "jh-hub")
	assert.True(t, service.IsValidationError(err), "a published Connection is refused by the service: %v", err)
	err = c.repo.Delete(ctx, "demo", "jh-hub")
	assert.True(t, apierrors.IsConflict(err), "%v", err)
}

func TestTheReaperKeepsTheSecretsWhenGitCannotBeRead(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	_, err := c.svc.Create(ctx, "demo", warehouse())
	require.NoError(t, err)
	require.NoError(t, c.svc.Delete(ctx, "demo", "warehouse"))

	moved := c.bare + ".away"
	require.NoError(t, os.Rename(c.bare, moved))
	err = c.reap(ctx)
	require.NoError(t, os.Rename(moved, c.bare))

	require.Error(t, err)
	assert.True(t, c.secretExists(t, "demo", "warehouse-credentials"), "a cached snapshot never decides a deletion")
}

func TestTheReaperKeepsTheSecretsWhenTheClusterConnectionsCannotBeListed(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	_, err := c.svc.Create(ctx, "demo", warehouse())
	require.NoError(t, err)
	c.applyConnection(t, "demo", "warehouse")
	require.NoError(t, c.svc.Delete(ctx, "demo", "warehouse"))

	// Transient API server failure listing cluster Connections: keep the Secret.
	c.cluster.PrependReactor("list", "connections", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	require.NoError(t, c.reap(ctx))
	assert.True(t, c.secretExists(t, "demo", "warehouse-credentials"))
}

func TestTheReaperTreatsAbsentConnectionCRDAsZeroClusterReferences(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	_, err := c.svc.Create(ctx, "demo", warehouse())
	require.NoError(t, err)
	require.NoError(t, c.svc.Delete(ctx, "demo", "warehouse"))

	c.cluster.PrependReactor("list", "connections", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(crd.GetConnectionGVR().GroupResource(), "")
	})
	require.NoError(t, c.reap(ctx))
	assert.False(t, c.secretExists(t, "demo", "warehouse-credentials"))
}

func TestConnectionListAndGetCopyObservedCreationTimestamp(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	_, err := c.svc.Create(ctx, "demo", warehouse())
	require.NoError(t, err)
	c.applyConnection(t, "demo", "warehouse")

	createdAt := time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)
	resource := c.cluster.Resource(crd.GetConnectionGVR()).Namespace("demo")
	obj, err := resource.Get(ctx, "warehouse", metav1.GetOptions{})
	require.NoError(t, err)
	obj.SetCreationTimestamp(metav1.NewTime(createdAt))
	_, err = resource.Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(t, err)

	got, err := c.repo.Get(ctx, "demo", "warehouse")
	require.NoError(t, err)
	assert.Equal(t, createdAt, got.CreationTimestamp.UTC())

	listed, err := c.svc.List(ctx, "demo")
	require.NoError(t, err)
	require.Len(t, listed, 1)
	parsed, err := time.Parse(time.RFC3339, listed[0].CreatedAt)
	require.NoError(t, err)
	assert.True(t, createdAt.Equal(parsed), "got %q", listed[0].CreatedAt)
}

func TestUpdateRotatesCredentialsSecretPreservingUnchangedKeys(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	_, err := c.svc.Create(ctx, "demo", warehouse())
	require.NoError(t, err)
	c.applyConnection(t, "demo", "warehouse")

	// Update only username; password is omitted and must be preserved in the rotated Secret.
	req := warehouse()
	req.Values["username"] = "writer"
	delete(req.Values, "password")
	resp, err := c.svc.Update(ctx, "demo", "warehouse", req)
	require.NoError(t, err)
	require.NotNil(t, resp.CredentialsSecret)
	rotated := resp.CredentialsSecret.Name
	assert.True(t, strings.HasPrefix(rotated, "warehouse-credentials-rot-"), "got %q", rotated)

	rotatedSecret, err := c.client.CoreV1().Secrets("demo").Get(ctx, rotated, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("writer"), rotatedSecret.Data["username"])
	assert.Equal(t, []byte("s3cret"), rotatedSecret.Data["password"], "unchanged credential key must be copied into the rotated Secret")

	// The previous live Secret stays untouched while the cluster Connection still points at it.
	oldSecret, err := c.client.CoreV1().Secrets("demo").Get(ctx, "warehouse-credentials", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("reader"), oldSecret.Data["username"])
	assert.Equal(t, "true", oldSecret.Labels[crd.LabelOrphanedCredentials])

	require.NoError(t, c.reap(ctx))
	assert.True(t, c.secretExists(t, "demo", "warehouse-credentials"), "kept until the cluster drops its reference")

	c.applyConnection(t, "demo", "warehouse")
	require.NoError(t, c.reap(ctx))
	assert.False(t, c.secretExists(t, "demo", "warehouse-credentials"), "reaped once the cluster applies the rotated reference")
	assert.True(t, c.secretExists(t, "demo", rotated))
}

func TestFailedUpdateReleasesRotatedSecretAndKeepsLiveCredentials(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	_, err := c.svc.Create(ctx, "demo", warehouse())
	require.NoError(t, err)
	c.applyConnection(t, "demo", "warehouse")

	// Reject the next git push via pre-receive hook so the commit fails after Secret rotation.
	hookPath := filepath.Join(c.bare, "hooks", "pre-receive")
	require.NoError(t, os.WriteFile(hookPath, []byte("#!/bin/sh\necho 'policy denied' >&2\nexit 1\n"), 0o755))

	req := warehouse()
	req.Values["password"] = "new-secret"
	_, err = c.svc.Update(ctx, "demo", "warehouse", req)
	require.Error(t, err)

	// Remove the hook so the reaper can read git cleanly.
	require.NoError(t, os.Remove(hookPath))

	live, err := c.client.CoreV1().Secrets("demo").Get(ctx, "warehouse-credentials", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("s3cret"), live.Data["password"], "live credentials must not be mutated in place")
	assert.Empty(t, live.Labels[crd.LabelOrphanedCredentials])

	// The rejected rotated Secret was marked orphaned and is cleaned up on reap.
	secretsBefore, err := c.client.CoreV1().Secrets("demo").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, secretsBefore.Items, 2)

	require.NoError(t, c.reap(ctx))
	secretsAfter, err := c.client.CoreV1().Secrets("demo").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, secretsAfter.Items, 1)
	assert.Equal(t, "warehouse-credentials", secretsAfter.Items[0].Name)
}

func TestCheckUpdateRefusesClusterOnlyConnectionBeforeTouchingSecrets(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	clusterOnly := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": crd.ConnectionAPIVersion, "kind": crd.ConnectionKind,
		"metadata": map[string]any{"name": "legacy-db", "namespace": "demo"},
		"spec":     map[string]any{"contract": "database-server", "values": map[string]any{"host": "db.example.com"}},
	}}
	_, err := c.cluster.Resource(crd.GetConnectionGVR()).Namespace("demo").Create(ctx, clusterOnly, metav1.CreateOptions{})
	require.NoError(t, err)

	req := warehouse()
	req.Name = "legacy-db"
	_, err = c.svc.Update(ctx, "demo", "legacy-db", req)
	require.Error(t, err)
	assert.True(t, apierrors.IsConflict(err), "%v", err)

	secrets, err := c.client.CoreV1().Secrets("demo").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, secrets.Items, "no Secret may be written when CheckUpdate rejects the update")
}

func TestCreateOrUpdateSecretMapsMissingNamespaceToProvisioningError(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	c.client.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "demo")
	})

	err := c.repo.CreateOrUpdateSecret(ctx, "demo", "warehouse-credentials", map[string][]byte{"password": []byte("s3cret")})
	require.Error(t, err)
	assert.ErrorIs(t, err, engine.ErrNamespaceProvisioning)
	assert.False(t, apierrors.IsNotFound(err), "must not be swallowed as a 404")
}

func TestAnUpdateBuiltFromAStaleReadIsAConflict(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	_, err := c.svc.Create(ctx, "demo", warehouse())
	require.NoError(t, err)
	stale, err := c.repo.Get(ctx, "demo", "warehouse")
	require.NoError(t, err)

	content := showFile(t, c.bare, "projects/demo/connections/warehouse.yaml")
	edited := strings.Replace(content, "annotations:\n", "annotations:\n    team: data\n", 1)
	require.NotEqual(t, content, edited)
	pushDirect(t, c.bare, map[string][]byte{storePath + "/projects/demo/connections/warehouse.yaml": []byte(edited + "\n")}, "annotate")

	stale.Spec.Description = "Moved"
	err = c.repo.Update(ctx, "demo", stale)
	assert.True(t, apierrors.IsConflict(err), "%v", err)

	// Through the service, which reads first: the key added by hand stays.
	req := warehouse()
	req.Description = "Moved"
	delete(req.Values, "password")
	_, err = c.svc.Update(ctx, "demo", "warehouse", req)
	require.NoError(t, err)
	content = showFile(t, c.bare, "projects/demo/connections/warehouse.yaml")
	assert.Contains(t, content, "team: data")
	assert.Contains(t, content, "description: Moved")
}

func TestTheReaperKeepsTheSecretsOfANamespaceWithAnUnreadableConnectionFile(t *testing.T) {
	for _, file := range []string{"broken.yaml", "Broken_Name.yml"} {
		t.Run(file, func(t *testing.T) {
			c := newConnections(t)
			ctx := testCtx(t)
			_, err := c.svc.Create(ctx, "demo", warehouse())
			require.NoError(t, err)
			require.NoError(t, c.svc.Delete(ctx, "demo", "warehouse"))
			corrupt := storePath + "/projects/demo/connections/" + file
			pushDirect(t, c.bare, map[string][]byte{corrupt: []byte("kind: Connection\nmetadata: [not, a, mapping\n")}, "a manifest that does not parse")

			require.NoError(t, c.reap(ctx))
			assert.True(t, c.secretExists(t, "demo", "warehouse-credentials"), "the unreadable file may reference it")

			listed, err := c.repo.List(ctx, "demo")
			require.NoError(t, err, "listing stays tolerant")
			assert.Empty(t, listed)

			pushDirect(t, c.bare, map[string][]byte{corrupt: nil}, "remove the broken manifest")
			require.NoError(t, c.reap(ctx))
			assert.False(t, c.secretExists(t, "demo", "warehouse-credentials"))
		})
	}
}

func TestGetSecretDataRefusesForeignSecret(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)

	_, err := c.client.CoreV1().Secrets("demo").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "foreign-secret", Namespace: "demo"},
		Data:       map[string][]byte{"password": []byte("top-secret")},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	_, err = c.repo.GetSecretData(ctx, "demo", "foreign-secret")
	require.Error(t, err)
	assert.ErrorIs(t, err, repository.ErrForeignSecret)
}

func TestUpdateCopiesOnlyContractSecretKeysAndRejectsForeignOrCrossNamespaceAnnotation(t *testing.T) {
	c := newConnections(t)
	ctx := testCtx(t)
	_, err := c.svc.Create(ctx, "demo", warehouse())
	require.NoError(t, err)

	// Inject an extra out-of-contract key into the managed credentials Secret.
	live, err := c.client.CoreV1().Secrets("demo").Get(ctx, "warehouse-credentials", metav1.GetOptions{})
	require.NoError(t, err)
	live.Data["extra-non-contract"] = []byte("must-not-propagate")
	_, err = c.client.CoreV1().Secrets("demo").Update(ctx, live, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Update only username; password is preserved, extra-non-contract must NOT be copied.
	req := warehouse()
	req.Values["username"] = "writer"
	delete(req.Values, "password")
	resp, err := c.svc.Update(ctx, "demo", "warehouse", req)
	require.NoError(t, err)
	require.NotNil(t, resp.CredentialsSecret)

	rotated, err := c.client.CoreV1().Secrets("demo").Get(ctx, resp.CredentialsSecret.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("writer"), rotated.Data["username"])
	assert.Equal(t, []byte("s3cret"), rotated.Data["password"])
	assert.NotContains(t, rotated.Data, "extra-non-contract", "arbitrary out-of-contract keys must never be copied")

	// Tamper the Connection in git to point to a foreign Secret in the same namespace.
	_, err = c.client.CoreV1().Secrets("demo").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "victim-secret", Namespace: "demo"},
		Data:       map[string][]byte{"password": []byte("stolen")},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	content := showFile(t, c.bare, "projects/demo/connections/warehouse.yaml")
	tampered := strings.Replace(content, "demo/"+resp.CredentialsSecret.Name, "demo/victim-secret", 1)
	require.NotEqual(t, content, tampered)
	pushDirect(t, c.bare, map[string][]byte{storePath + "/projects/demo/connections/warehouse.yaml": []byte(tampered)}, "point to foreign secret")

	_, err = c.svc.Update(ctx, "demo", "warehouse", req)
	require.Error(t, err)
	assert.True(t, service.IsValidationError(err), "foreign secret read must fail with validation error: %v", err)

	// Tamper the Connection in git to point to a Secret in ANOTHER namespace.
	_, err = c.client.CoreV1().Secrets("other-ns").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cross-ns-secret",
			Namespace: "other-ns",
			Labels:    map[string]string{crd.LabelManagedBy: crd.ManagedByValue},
		},
		Data: map[string][]byte{"password": []byte("other-ns-pass")},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	crossNS := strings.Replace(tampered, "demo/victim-secret", "other-ns/cross-ns-secret", 1)
	pushDirect(t, c.bare, map[string][]byte{storePath + "/projects/demo/connections/warehouse.yaml": []byte(crossNS)}, "point to cross-ns secret")

	resp2, err := c.svc.Update(ctx, "demo", "warehouse", req)
	require.NoError(t, err)
	require.NotNil(t, resp2.CredentialsSecret)
	rotated2, err := c.client.CoreV1().Secrets("demo").Get(ctx, resp2.CredentialsSecret.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, rotated2.Data, "password", "cross-namespace secret must never be read")
}
