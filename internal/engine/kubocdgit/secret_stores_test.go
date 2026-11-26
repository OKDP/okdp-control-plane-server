package kubocdgit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocdgit"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

type secretsEnv struct {
	*env
	client      *fake.Clientset
	stores      *kubocdgit.SecretStoreRepository
	externals   *kubocdgit.ExternalSecretRepository
	connections *kubocdgit.ConnectionRepository
	svc         *service.DefaultSecretStoreService
	esSvc       *service.DefaultExternalSecretService
}

func newSecrets(t *testing.T) *secretsEnv {
	t.Helper()
	e := newEnv(t)
	client := fake.NewSimpleClientset()
	client.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{
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
	stores := kubocdgit.NewSecretStoreRepository(e.adapter, repository.NewSecretStoreRepository(e.cluster, client.Discovery()), client)
	externals := kubocdgit.NewExternalSecretRepository(e.adapter, repository.NewExternalSecretRepository(e.cluster, client.Discovery()))
	connections := kubocdgit.NewConnectionRepository(e.adapter, repository.NewConnectionRepository(e.cluster, client, client.Discovery()), client)
	return &secretsEnv{
		env: e, client: client, stores: stores, externals: externals, connections: connections,
		svc:   service.NewDefaultSecretStoreService(stores),
		esSvc: service.NewDefaultExternalSecretService(externals, stores),
	}
}

func (s *secretsEnv) reap(ctx context.Context) error {
	return kubocdgit.NewCredentialsReaper(s.adapter, s.client, s.connections, s.stores).Reap(ctx)
}

func (s *secretsEnv) secretExists(t *testing.T, namespace, name string) bool {
	t.Helper()
	_, err := s.client.CoreV1().Secrets(namespace).Get(testCtx(t), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func (s *secretsEnv) secretLabels(t *testing.T, namespace, name string) map[string]string {
	t.Helper()
	secret, err := s.client.CoreV1().Secrets(namespace).Get(testCtx(t), name, metav1.GetOptions{})
	require.NoError(t, err)
	return secret.Labels
}

// applyObject plays the sync tool for one object: the cluster gets the file
// at the head of the branch, or loses it when the file is gone.
func (s *secretsEnv) applyObject(t *testing.T, gvr schema.GroupVersionResource, dir, namespace, name string) {
	t.Helper()
	ctx := testCtx(t)
	resource := s.cluster.Resource(gvr).Namespace(namespace)
	snap, err := newStore(t, s.bare).Latest(ctx)
	require.NoError(t, err)
	data, err := snap.ReadFile("projects/" + namespace + "/" + dir + "/" + name + ".yaml")
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
	desired.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	if current, err := resource.Get(ctx, name, metav1.GetOptions{}); err == nil {
		desired.SetResourceVersion(current.GetResourceVersion())
		_, err = resource.Update(ctx, desired, metav1.UpdateOptions{})
		require.NoError(t, err)
		return
	}
	_, err = resource.Create(ctx, desired, metav1.CreateOptions{})
	require.NoError(t, err)
}

func (s *secretsEnv) applyStore(t *testing.T, namespace, name string) {
	s.applyObject(t, crd.GetSecretStoreGVR(), "secret-stores", namespace, name)
}

func vaultStore(name, token string, isDefault bool) models.SecretStoreRequest {
	return models.SecretStoreRequest{
		Name:      name,
		Provider:  "vault",
		IsDefault: isDefault,
		Vault:     &models.VaultConfig{Server: "https://vault.example.com", Path: "secret", Version: "v2", CABundle: "-----BEGIN CERTIFICATE-----"},
		Auth:      &models.SecretStoreAuth{Type: "token", Config: models.SecretAuthConfig{Token: token}},
	}
}

func kubernetesAuth(name string) models.SecretStoreRequest {
	req := vaultStore(name, "", false)
	req.Auth = &models.SecretStoreAuth{Type: "kubernetes", Config: models.SecretAuthConfig{Role: "reader"}}
	return req
}

func storeFile(name string) string { return "projects/demo/secret-stores/" + name + ".yaml" }

func TestASecretStoreIsCommittedAndItsTokenStaysInTheCluster(t *testing.T) {
	s := newSecrets(t)
	ctx := testCtx(t)

	resp, err := s.svc.CreateSecretStore(ctx, "demo", vaultStore("vault", "hvs.TOKEN", true))
	require.NoError(t, err)
	assert.Equal(t, "vault", resp.Provider)
	assert.True(t, resp.IsDefault)
	assert.Equal(t, "token", resp.Auth.Type)

	content := showFile(t, s.bare, storeFile("vault"))
	var manifest map[string]any
	require.NoError(t, sigsyaml.Unmarshal([]byte(content), &manifest))
	assert.Equal(t, crd.SecretStoreKind, manifest["kind"])
	assert.NotContains(t, manifest, "status")
	assert.Contains(t, content, "tokenSecretRef:")
	assert.Contains(t, content, "name: vault-credentials")
	assert.Contains(t, content, "caBundle: LS0tLS1CRUdJTi", "the CA bundle is base64, the format the CRD declares")

	tree := runGit(t, s.bare, "ls-tree", "-r", "--name-only", "main")
	assert.NotContains(t, tree, "credentials", "the Secret never goes to git")
	assert.NotContains(t, runGit(t, s.bare, "log", "-p", "main"), "hvs.TOKEN")
	assert.Equal(t, crd.ManagedByValue, s.secretLabels(t, "demo", "vault-credentials")[crd.LabelManagedBy])

	_, err = s.cluster.Resource(crd.GetSecretStoreGVR()).Namespace("demo").Get(ctx, "vault", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "nothing written to the cluster: the sync tool applies it")

	s.applyStore(t, "demo", "vault")
	status, err := s.svc.GetSecretStoreStatus(ctx, "demo", "vault")
	require.NoError(t, err)
	assert.Equal(t, "Ready", status.Status, "the status comes from the cluster")
}

func TestAnUpdateKeepsTheFieldsTheConsoleDoesNotEdit(t *testing.T) {
	s := newSecrets(t)
	ctx := testCtx(t)
	_, err := s.svc.CreateSecretStore(ctx, "demo", vaultStore("vault", "hvs.TOKEN", false))
	require.NoError(t, err)

	content := showFile(t, s.bare, storeFile("vault"))
	edited := strings.Replace(content, "spec:\n", "spec:\n  refreshInterval: 1h\n", 1)
	edited = strings.Replace(edited, "    vault:\n", "    vault:\n      namespace: team-a\n", 1)
	edited = strings.Replace(edited, "metadata:\n", "metadata:\n  labels:\n    team: data\n    kustomize.toolkit.fluxcd.io/name: okdp\n  uid: abc\n", 1)
	edited += "\nstatus:\n  conditions: []\n"
	pushDirect(t, s.bare, map[string][]byte{storePath + "/" + storeFile("vault"): []byte(edited)}, "edit by hand")

	req := kubernetesAuth("vault")
	req.Vault.Server = "https://vault2.example.com"
	_, err = s.svc.UpdateSecretStore(ctx, "demo", "vault", req)
	require.NoError(t, err)

	content = showFile(t, s.bare, storeFile("vault"))
	assert.Contains(t, content, "refreshInterval: 1h")
	assert.Contains(t, content, "namespace: team-a")
	assert.Contains(t, content, "team: data")
	assert.Contains(t, content, "server: https://vault2.example.com")
	assert.Contains(t, content, "role: reader")
	assert.NotContains(t, content, "tokenSecretRef")
	assert.NotContains(t, content, "kustomize.toolkit.fluxcd.io")
	assert.NotContains(t, content, "uid:")
	assert.NotContains(t, content, "status:")
}

func TestTheDefaultLabelMovesInTheWriteThatSetsIt(t *testing.T) {
	s := newSecrets(t)
	ctx := testCtx(t)
	_, err := s.svc.CreateSecretStore(ctx, "demo", vaultStore("first", "a", true))
	require.NoError(t, err)

	before := runGit(t, s.bare, "rev-list", "--count", "main")
	_, err = s.svc.CreateSecretStore(ctx, "demo", vaultStore("second", "b", true))
	require.NoError(t, err)
	assert.Equal(t, before, runGit(t, s.bare, "rev-list", "--count", "main^"), "one commit creates the store and moves the label")
	assert.NotContains(t, showFile(t, s.bare, storeFile("first")), crd.LabelDefaultStore)
	assert.Contains(t, showFile(t, s.bare, storeFile("second")), crd.LabelDefaultStore)

	// Updating the default store as default does not conflict with itself.
	req := vaultStore("second", "", true)
	req.Vault.Path = "kv"
	_, err = s.svc.UpdateSecretStore(ctx, "demo", "second", req)
	require.NoError(t, err)
	assert.Contains(t, showFile(t, s.bare, storeFile("second")), crd.LabelDefaultStore)

	require.NoError(t, s.stores.RemoveDefaultLabel(ctx, "demo"))
	assert.NotContains(t, showFile(t, s.bare, storeFile("second")), crd.LabelDefaultStore)
	listed, err := s.svc.ListSecretStores(ctx, "demo")
	require.NoError(t, err)
	require.Len(t, listed, 2)
	assert.False(t, listed[0].IsDefault || listed[1].IsDefault)
}

func TestTheTokenOfADeletedStoreOutlivesItUntilTheClusterDropsIt(t *testing.T) {
	s := newSecrets(t)
	ctx := testCtx(t)
	_, err := s.svc.CreateSecretStore(ctx, "demo", vaultStore("vault", "hvs.TOKEN", false))
	require.NoError(t, err)
	s.applyStore(t, "demo", "vault")

	require.NoError(t, s.svc.DeleteSecretStore(ctx, "demo", "vault"))
	assert.NotContains(t, runGit(t, s.bare, "ls-tree", "-r", "--name-only", "main"), "vault.yaml")
	require.True(t, s.secretExists(t, "demo", "vault-credentials"), "not deleted when the deletion is committed")
	assert.Equal(t, "true", s.secretLabels(t, "demo", "vault-credentials")[crd.LabelOrphanedCredentials])

	require.NoError(t, s.reap(ctx))
	assert.True(t, s.secretExists(t, "demo", "vault-credentials"), "the cluster SecretStore still reads it")

	s.applyStore(t, "demo", "vault")
	require.NoError(t, s.reap(ctx))
	assert.False(t, s.secretExists(t, "demo", "vault-credentials"))
}

func TestSwitchingToKubernetesAuthReleasesTheToken(t *testing.T) {
	s := newSecrets(t)
	ctx := testCtx(t)
	_, err := s.svc.CreateSecretStore(ctx, "demo", vaultStore("vault", "hvs.TOKEN", false))
	require.NoError(t, err)
	s.applyStore(t, "demo", "vault")

	_, err = s.svc.UpdateSecretStore(ctx, "demo", "vault", kubernetesAuth("vault"))
	require.NoError(t, err)
	require.NoError(t, s.reap(ctx))
	assert.True(t, s.secretExists(t, "demo", "vault-credentials"), "kept until the change is applied")

	s.applyStore(t, "demo", "vault")
	require.NoError(t, s.reap(ctx))
	assert.False(t, s.secretExists(t, "demo", "vault-credentials"))
}

func TestAStoreCreatedAgainKeepsItsToken(t *testing.T) {
	s := newSecrets(t)
	ctx := testCtx(t)
	_, err := s.svc.CreateSecretStore(ctx, "demo", vaultStore("vault", "one", false))
	require.NoError(t, err)
	require.NoError(t, s.svc.DeleteSecretStore(ctx, "demo", "vault"))
	_, err = s.svc.CreateSecretStore(ctx, "demo", vaultStore("vault", "two", false))
	require.NoError(t, err)

	assert.NotContains(t, s.secretLabels(t, "demo", "vault-credentials"), crd.LabelOrphanedCredentials)
	require.NoError(t, s.reap(ctx))
	assert.True(t, s.secretExists(t, "demo", "vault-credentials"))
}

func TestTheReaperKeepsTheSecretsOfANamespaceWithAnUnreadableStoreFile(t *testing.T) {
	for _, file := range []string{"broken.yaml", "Broken_Name.yml"} {
		t.Run(file, func(t *testing.T) {
			s := newSecrets(t)
			ctx := testCtx(t)
			_, err := s.svc.CreateSecretStore(ctx, "demo", vaultStore("vault", "hvs.TOKEN", false))
			require.NoError(t, err)
			require.NoError(t, s.svc.DeleteSecretStore(ctx, "demo", "vault"))
			corrupt := storePath + "/projects/demo/secret-stores/" + file
			pushDirect(t, s.bare, map[string][]byte{corrupt: []byte("kind: SecretStore\nmetadata: [not, a, mapping\n")}, "a manifest that does not parse")

			require.NoError(t, s.reap(ctx))
			assert.True(t, s.secretExists(t, "demo", "vault-credentials"), "the unreadable file may reference it")

			pushDirect(t, s.bare, map[string][]byte{corrupt: nil}, "remove the broken manifest")
			require.NoError(t, s.reap(ctx))
			assert.False(t, s.secretExists(t, "demo", "vault-credentials"))
		})
	}
}

func TestTheReaperSeesEveryDocumentOfAStoreFile(t *testing.T) {
	s := newSecrets(t)
	ctx := testCtx(t)
	_, err := s.svc.CreateSecretStore(ctx, "demo", vaultStore("vault", "hvs.TOKEN", false))
	require.NoError(t, err)
	require.NoError(t, s.svc.DeleteSecretStore(ctx, "demo", "vault"))
	other := "apiVersion: " + crd.SecretStoreAPIVersion + "\nkind: SecretStore\nmetadata:\n  name: other\n  namespace: demo\nspec:\n  provider:\n    vault:\n      server: https://vault.example.com\n      auth:\n        tokenSecretRef:\n          name: other-credentials\n          key: token\n"
	hidden := strings.Replace(other, "other-credentials", "vault-credentials", 1)
	hidden = strings.Replace(hidden, "name: other\n", "name: hidden\n", 1)
	pushDirect(t, s.bare, map[string][]byte{storePath + "/projects/demo/secret-stores/other.yaml": []byte(other + "---\n" + hidden)}, "two stores in one file")

	require.NoError(t, s.reap(ctx))
	assert.True(t, s.secretExists(t, "demo", "vault-credentials"), "the second document references it")
}

func TestAConnectionKeepsATokenItReferences(t *testing.T) {
	s := newSecrets(t)
	ctx := testCtx(t)
	_, err := s.svc.CreateSecretStore(ctx, "demo", vaultStore("vault", "hvs.TOKEN", false))
	require.NoError(t, err)
	require.NoError(t, s.svc.DeleteSecretStore(ctx, "demo", "vault"))
	pushDirect(t, s.bare, map[string][]byte{storePath + "/projects/demo/connections/vault.yaml": []byte(
		"apiVersion: " + crd.ConnectionAPIVersion + "\nkind: Connection\nmetadata:\n  name: vault\n  namespace: demo\nspec:\n  contract: vault\n  values:\n    secretRef: vault-credentials\n")}, "a connection on the token")

	require.NoError(t, s.reap(ctx))
	assert.True(t, s.secretExists(t, "demo", "vault-credentials"))
}

func TestAStoreOnlyInTheClusterIsListedButItsTokenIsNeverRewritten(t *testing.T) {
	s := newSecrets(t)
	ctx := testCtx(t)
	_, err := s.client.CoreV1().Secrets("demo").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-credentials", Namespace: "demo"},
		Data:       map[string][]byte{"token": []byte("live")},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	legacy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": crd.SecretStoreAPIVersion, "kind": crd.SecretStoreKind,
		"metadata": map[string]any{"name": "legacy", "namespace": "demo"},
		"spec": map[string]any{"provider": map[string]any{"vault": map[string]any{
			"server": "https://vault.example.com", "path": "secret", "version": "v2",
			"auth": map[string]any{"tokenSecretRef": map[string]any{"name": "legacy-credentials", "key": "token"}},
		}}},
	}}
	_, err = s.cluster.Resource(crd.GetSecretStoreGVR()).Namespace("demo").Create(ctx, legacy, metav1.CreateOptions{})
	require.NoError(t, err)

	listed, err := s.svc.ListSecretStores(ctx, "demo")
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "legacy", listed[0].Name)

	_, err = s.svc.UpdateSecretStore(ctx, "demo", "legacy", vaultStore("legacy", "rewritten", false))
	assert.True(t, apierrors.IsConflict(err), "%v", err)
	_, err = s.svc.CreateSecretStore(ctx, "demo", vaultStore("legacy", "rewritten", false))
	assert.True(t, apierrors.IsAlreadyExists(err), "%v", err)
	secret, err := s.client.CoreV1().Secrets("demo").Get(ctx, "legacy-credentials", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "live", string(secret.Data["token"]), "a refused write leaves the live token alone")
}

func TestATokenNeverTakesOverASecretTheControlPlaneDoesNotOwn(t *testing.T) {
	s := newSecrets(t)
	ctx := testCtx(t)
	_, err := s.client.CoreV1().Secrets("demo").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-credentials", Namespace: "demo"},
		Data:       map[string][]byte{"token": []byte("someone else's")},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	_, err = s.svc.CreateSecretStore(ctx, "demo", vaultStore("vault", "mine", false))

	require.Error(t, err)
	secret, err := s.client.CoreV1().Secrets("demo").Get(ctx, "vault-credentials", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "someone else's", string(secret.Data["token"]))
	assert.NotContains(t, secret.Labels, crd.LabelManagedBy)
	assert.NotContains(t, runGit(t, s.bare, "ls-tree", "-r", "--name-only", "main"), "vault.yaml")
}

func TestAnUpdateWithoutATokenKeepsTheSecretTheStoreReads(t *testing.T) {
	s := newSecrets(t)
	ctx := testCtx(t)
	pushDirect(t, s.bare, map[string][]byte{storePath + "/" + storeFile("vault"): []byte(
		"apiVersion: " + crd.SecretStoreAPIVersion + "\nkind: SecretStore\nmetadata:\n  name: vault\n  namespace: demo\nspec:\n  provider:\n    vault:\n" +
			"      server: https://vault.example.com\n      path: secret\n      version: v2\n      auth:\n        tokenSecretRef:\n          name: shared-token\n          key: t\n")}, "a store written by hand")

	req := vaultStore("vault", "", false)
	req.Vault.Path = "kv"
	_, err := s.svc.UpdateSecretStore(ctx, "demo", "vault", req)
	require.NoError(t, err)
	content := showFile(t, s.bare, storeFile("vault"))
	assert.Contains(t, content, "name: shared-token")
	assert.Contains(t, content, "key: t")
	assert.Contains(t, content, "path: kv")
}

func TestAnExternalSecretIsCommittedAndKeepsItsUnknownFields(t *testing.T) {
	s := newSecrets(t)
	ctx := testCtx(t)
	_, err := s.svc.CreateSecretStore(ctx, "demo", vaultStore("vault", "hvs.TOKEN", false))
	require.NoError(t, err)

	req := models.ExternalSecretRequest{
		Name:            "db",
		SecretStoreRef:  "vault",
		RefreshInterval: "1h",
		Target:          models.ExternalSecretTarget{Name: "db-secret"},
		Data: []models.ExternalSecretDataEntry{
			{SecretKey: "user", RemoteRef: models.ExternalSecretRemote{Key: "db", Property: "user"}},
			{SecretKey: "password", RemoteRef: models.ExternalSecretRemote{Key: "db", Property: "password"}},
		},
	}
	_, err = s.esSvc.CreateExternalSecret(ctx, "demo", req)
	require.NoError(t, err)
	p := "projects/demo/external-secrets/db.yaml"
	content := showFile(t, s.bare, p)
	assert.Contains(t, content, "kind: ExternalSecret")
	assert.Contains(t, content, "name: db-secret")
	_, err = s.cluster.Resource(crd.GetExternalSecretGVR()).Namespace("demo").Get(ctx, "db", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))

	edited := strings.Replace(content, "    - remoteRef:\n        key: db\n        property: user\n", "    - remoteRef:\n        key: db\n        property: user\n        decodingStrategy: Base64\n", 1)
	require.NotEqual(t, content, edited)
	edited = strings.Replace(edited, "spec:\n", "spec:\n  dataFrom:\n  - extract:\n      key: extra\n", 1)
	pushDirect(t, s.bare, map[string][]byte{storePath + "/" + p: []byte(edited)}, "edit by hand")

	req.Data = req.Data[:1]
	req.RefreshInterval = "15m"
	_, err = s.esSvc.UpdateExternalSecret(ctx, "demo", "db", req)
	require.NoError(t, err)
	content = showFile(t, s.bare, p)
	assert.Contains(t, content, "decodingStrategy: Base64", "the entry with the same secretKey keeps its fields")
	assert.Contains(t, content, "key: extra")
	assert.Contains(t, content, "refreshInterval: 15m")
	assert.NotContains(t, content, "secretKey: password")

	s.applyObject(t, crd.GetExternalSecretGVR(), "external-secrets", "demo", "db")
	status, err := s.esSvc.GetExternalSecretStatus(ctx, "demo", "db")
	require.NoError(t, err)
	assert.NotEmpty(t, status.Conditions)

	require.NoError(t, s.esSvc.DeleteExternalSecret(ctx, "demo", "db"))
	assert.NotContains(t, runGit(t, s.bare, "ls-tree", "-r", "--name-only", "main"), "db.yaml")
}

func TestARotatedTokenNeverRewritesASecretAStoreMayRead(t *testing.T) {
	s := newSecrets(t)
	ctx := testCtx(t)
	_, err := s.svc.CreateSecretStore(ctx, "demo", vaultStore("vault", "one", false))
	require.NoError(t, err)
	s.applyStore(t, "demo", "vault")
	token := func(name string) string {
		secret, err := s.client.CoreV1().Secrets("demo").Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)
		return string(secret.Data["token"])
	}
	referenced := func() string {
		store, err := s.stores.Get(ctx, "demo", "vault")
		require.NoError(t, err)
		return store.Spec.Provider.Vault.Auth.TokenSecretRef.Name
	}

	// Two rotations before the sync tool applies either.
	_, err = s.svc.UpdateSecretStore(ctx, "demo", "vault", vaultStore("vault", "two", false))
	require.NoError(t, err)
	second := referenced()
	_, err = s.svc.UpdateSecretStore(ctx, "demo", "vault", vaultStore("vault", "three", false))
	require.NoError(t, err)
	third := referenced()

	assert.NotEqual(t, second, third)
	assert.Equal(t, "one", token("vault-credentials"), "the token the cluster store reads is never rewritten")
	assert.Equal(t, "two", token(second))
	assert.Equal(t, "three", token(third))
	assert.Equal(t, crd.ManagedByValue, s.secretLabels(t, "demo", third)[crd.LabelManagedBy], "the reaper knows it")

	require.NoError(t, s.reap(ctx))
	assert.True(t, s.secretExists(t, "demo", "vault-credentials"), "the cluster store still reads it")
	assert.False(t, s.secretExists(t, "demo", second), "released and read by nothing")
	s.applyStore(t, "demo", "vault")
	require.NoError(t, s.reap(ctx))
	assert.False(t, s.secretExists(t, "demo", "vault-credentials"))
	assert.Equal(t, "three", token(third))
}
