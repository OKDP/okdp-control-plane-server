// Package chart holds the tests of the Helm chart rendering.
package chart

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	release   = "okdp"
	namespace = "okdp-system"
	fullname  = "okdp-okdp-control-plane-server"
)

type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec  map[string]any `yaml:"spec"`
	Data  map[string]any `yaml:"data"`
	Rules []rule         `yaml:"rules"`
	raw   map[string]any
}

type rule struct {
	APIGroups     []string `yaml:"apiGroups"`
	Resources     []string `yaml:"resources"`
	ResourceNames []string `yaml:"resourceNames"`
	Verbs         []string `yaml:"verbs"`
}

// decodeAll reads a multi-document stream. yaml.v3 refuses a mapping that
// defines a key twice, which a sloppy template renders silently.
func decodeAll(t *testing.T, data []byte) []object {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []object
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		if len(node.Content) == 0 {
			continue
		}
		var obj object
		require.NoError(t, node.Decode(&obj))
		require.NoError(t, node.Decode(&obj.raw))
		out = append(out, obj)
	}
}

func render(t *testing.T, set ...string) []object {
	t.Helper()
	out, err := helmTemplate(t, set...)
	require.NoError(t, err, "%s", out)
	return decodeAll(t, out)
}

func helmTemplate(t *testing.T, set ...string) ([]byte, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed, the rendering is not checked")
	}
	args := []string{"template", release, ".", "--namespace", namespace}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	cmd := exec.Command(helm, args...)
	cmd.Env = append(os.Environ(), "HELM_CACHE_HOME="+t.TempDir(), "HELM_CONFIG_HOME="+t.TempDir(), "HELM_DATA_HOME="+t.TempDir())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return stderr.Bytes(), err
	}
	return out, nil
}

func find(objects []object, kind, name string) *object {
	for i := range objects {
		if objects[i].Kind == kind && objects[i].Metadata.Name == name {
			return &objects[i]
		}
	}
	return nil
}

func gitMode(extra ...string) []string {
	return append([]string{
		"engine.kubocd.persistence.mode=git",
		"engine.kubocd.persistence.git.url=https://forge.example/okdp/deployments.git",
	}, extra...)
}

// verbs returns the verbs the rules grant on resource of group.
func verbs(rules []rule, group, resource string) []string {
	var out []string
	for _, r := range rules {
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				if g == group && res == resource {
					out = append(out, r.Verbs...)
				}
			}
		}
	}
	return out
}

func syncObjects(objects []object) []*object {
	var out []*object
	for _, ref := range []struct{ kind, name string }{
		{"ServiceAccount", fullname + "-gitops"},
		{"ClusterRole", fullname + "-gitops"},
		{"ClusterRoleBinding", fullname + "-gitops"},
		{"GitRepository", fullname},
		{"Kustomization", fullname},
	} {
		if obj := find(objects, ref.kind, ref.name); obj != nil {
			out = append(out, obj)
		}
	}
	return out
}

func TestValuesParse(t *testing.T) {
	data, err := os.ReadFile("values.yaml")
	require.NoError(t, err)
	var values map[string]any
	require.NoError(t, yaml.Unmarshal(data, &values))
}

func TestKubernetesModeKeepsEveryRightOfTheServer(t *testing.T) {
	objects := render(t)

	golden, err := os.ReadFile("testdata/kubernetes-mode-rbac.yaml")
	require.NoError(t, err)
	want := find(decodeAll(t, golden), "ClusterRole", fullname)
	require.NotNil(t, want)
	got := find(objects, "ClusterRole", fullname)
	require.NotNil(t, got)
	assert.Equal(t, want.Rules, got.Rules)

	assert.Empty(t, syncObjects(objects), "no Flux object in kubernetes mode")
	role := find(objects, "Role", fullname+"-gitops-sync")
	require.NotNil(t, role, "the server may delete the sync objects left by git mode")
	assert.Equal(t, []string{"get", "delete"}, verbs(role.Rules, "kustomize.toolkit.fluxcd.io", "kustomizations"), "the startup cleanup only")
	assert.Equal(t, []string{"get", "delete"}, verbs(role.Rules, "source.toolkit.fluxcd.io", "gitrepositories"), "the startup cleanup only")
	for _, r := range role.Rules {
		assert.Equal(t, []string{fullname}, r.ResourceNames, "only the objects named after the release")
	}
	require.NotNil(t, find(objects, "RoleBinding", fullname+"-gitops-sync"))

	cm := find(objects, "ConfigMap", fullname)
	require.NotNil(t, cm)
	assert.Equal(t, "control-plane", cm.Data["CONTROL_PLANE_CONTEXT_NAME"])
	assert.Equal(t, "", cm.Data["CONTROL_PLANE_CONTEXT_NAMESPACE"])
	assert.Equal(t, "platform", cm.Data["PLATFORM_CONTEXT_NAME"])
	assert.Equal(t, "", cm.Data["PLATFORM_CONTEXT_NAMESPACE"])
	assert.Equal(t, "kubernetes", cm.Data["ENGINE_KUBOCD_PERSISTENCE_MODE"])
	assert.Equal(t, fullname, cm.Data["ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC_CLEANUP_NAME"])
	assert.Equal(t, namespace, cm.Data["ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC_NAMESPACE"])

	deployment := find(objects, "Deployment", fullname)
	require.NotNil(t, deployment)
	podSpec := deployment.raw["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	container := podSpec["containers"].([]any)[0].(map[string]any)
	assert.Equal(t, map[string]any{
		"runAsNonRoot":   true,
		"runAsUser":      65534,
		"runAsGroup":     65534,
		"fsGroup":        65534,
		"seccompProfile": map[string]any{"type": "RuntimeDefault"},
	}, podSpec["securityContext"])
	assert.Equal(t, map[string]any{
		"allowPrivilegeEscalation": false,
		"capabilities":             map[string]any{"drop": []any{"ALL"}},
		"readOnlyRootFilesystem":   true,
	}, container["securityContext"])
	assert.Equal(t, []any{map[string]any{"name": "tmp", "mountPath": "/tmp"}}, container["volumeMounts"])
	assert.Equal(t, []any{map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "256Mi"}}}, podSpec["volumes"])
}

func TestGitModeRendersTheSyncObjectsAndRestrictsTheServer(t *testing.T) {
	objects := render(t, gitMode(
		"engine.kubocd.persistence.git.path=clusters/prod",
		"engine.kubocd.persistence.git.secretName=git-credentials",
	)...)

	sync := syncObjects(objects)
	require.Len(t, sync, 5)
	for _, obj := range sync {
		assert.Equal(t, fullname, obj.Metadata.Labels["app.kubernetes.io/managed-by-control-plane"], obj.Kind)
		assert.Equal(t, "keep", obj.Metadata.Annotations["helm.sh/resource-policy"], obj.Kind)
	}

	repo := find(objects, "GitRepository", fullname)
	assert.Equal(t, namespace, repo.Metadata.Namespace)
	assert.Equal(t, "https://forge.example/okdp/deployments.git", repo.Spec["url"])
	assert.Equal(t, map[string]any{"branch": "main"}, repo.Spec["ref"])
	assert.Equal(t, "1m", repo.Spec["interval"])
	assert.Equal(t, map[string]any{"name": "git-credentials"}, repo.Spec["secretRef"])

	ks := find(objects, "Kustomization", fullname)
	assert.Equal(t, namespace, ks.Metadata.Namespace)
	assert.Equal(t, map[string]any{"kind": "GitRepository", "name": fullname}, ks.Spec["sourceRef"])
	assert.Equal(t, "./clusters/prod", ks.Spec["path"])
	assert.Equal(t, true, ks.Spec["prune"])
	assert.Equal(t, false, ks.Spec["wait"])
	assert.Equal(t, "Orphan", ks.Spec["deletionPolicy"])
	assert.Equal(t, fullname+"-gitops", ks.Spec["serviceAccountName"])

	role := find(objects, "Role", fullname+"-gitops-sync")
	require.NotNil(t, role)
	assert.Equal(t, []string{"get", "list", "watch", "patch"}, verbs(role.Rules, "source.toolkit.fluxcd.io", "gitrepositories"), "read and reconcile request, never delete")
	assert.Equal(t, []string{"get", "list", "watch"}, verbs(role.Rules, "kustomize.toolkit.fluxcd.io", "kustomizations"), "read only")
	for _, r := range role.Rules {
		assert.Equal(t, []string{fullname}, r.ResourceNames)
	}

	syncRole := find(objects, "ClusterRole", fullname+"-gitops")
	all := []string{"get", "list", "watch", "create", "update", "patch", "delete"}
	assert.Equal(t, all, verbs(syncRole.Rules, "", "namespaces"))
	for _, res := range []string{"contexts", "connections", "releases"} {
		assert.Equal(t, all, verbs(syncRole.Rules, "kubocd.kubotal.io", res), res)
	}
	for _, res := range []string{"secretstores", "externalsecrets"} {
		assert.Equal(t, all, verbs(syncRole.Rules, "external-secrets.io", res), res)
	}
	assert.Len(t, syncRole.Rules, 3, "nothing else: no Secrets, no pods")

	server := find(objects, "ClusterRole", fullname).Rules
	read := []string{"get", "list", "watch"}
	assert.Equal(t, read, verbs(server, "", "namespaces"))
	assert.Equal(t, []string{"get", "list"}, verbs(server, "", "pods"), "git mode drops pod deletion")
	assert.Equal(t, []string{"get", "list"}, verbs(server, "", "persistentvolumeclaims"))
	assert.Equal(t, []string{"get", "list"}, verbs(server, "kubauth.kubotal.io", "oidcclients"), "git mode drops OIDC client deletion")
	for _, res := range []string{"releases", "contexts", "connections"} {
		assert.Equal(t, read, verbs(server, "kubocd.kubotal.io", res), res)
	}
	for _, res := range []string{"secretstores", "externalsecrets"} {
		assert.Equal(t, read, verbs(server, "external-secrets.io", res), res)
	}
	assert.Contains(t, verbs(server, "", "secrets"), "delete", "the credentials Secrets stay in the cluster")
	assert.Equal(t, read, verbs(server, "kubocd.kubotal.io", "clusterconnections"), "ClusterConnections are read-only")
	assert.Contains(t, verbs(server, "kubauth.kubotal.io", "users"), "create")
	assert.Contains(t, verbs(server, "sparkoperator.k8s.io", "sparkapplications"), "create")

	cm := find(objects, "ConfigMap", fullname)
	assert.Equal(t, "git", cm.Data["ENGINE_KUBOCD_PERSISTENCE_MODE"])
	assert.Equal(t, "https://forge.example/okdp/deployments.git", cm.Data["ENGINE_KUBOCD_PERSISTENCE_GIT_URL"])
	assert.Equal(t, "clusters/prod", cm.Data["ENGINE_KUBOCD_PERSISTENCE_GIT_PATH"])
	assert.Equal(t, "flux", cm.Data["ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC"])
	assert.Equal(t, "true", cm.Data["ENGINE_KUBOCD_PERSISTENCE_GIT_INITIALIZE"])
	assert.Equal(t, "/etc/okdp/git", cm.Data["ENGINE_KUBOCD_PERSISTENCE_GIT_CREDENTIALS_DIR"])
	assert.Equal(t, fullname, cm.Data["ENGINE_KUBOCD_PERSISTENCE_GIT_REPOSITORY_NAME"])
	assert.Equal(t, fullname, cm.Data["ENGINE_KUBOCD_PERSISTENCE_GIT_KUSTOMIZATION_NAME"])

	podSpec := find(objects, "Deployment", fullname).raw["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	container := podSpec["containers"].([]any)[0].(map[string]any)
	assert.Equal(t, []any{
		map[string]any{"name": "tmp", "mountPath": "/tmp"},
		map[string]any{"name": "git-credentials", "mountPath": "/etc/okdp/git", "readOnly": true},
	}, container["volumeMounts"])
	assert.Equal(t, []any{
		map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "256Mi"}},
		map[string]any{"name": "git-credentials", "secret": map[string]any{"secretName": "git-credentials", "defaultMode": 288}},
	}, podSpec["volumes"])
}

func TestTheSyncObjectsAreReleasedOnUninstallOnlyWhenAsked(t *testing.T) {
	objects := render(t, gitMode("engine.kubocd.persistence.git.keepSyncOnUninstall=false")...)

	sync := syncObjects(objects)
	require.Len(t, sync, 5)
	for _, obj := range sync {
		assert.NotContains(t, obj.Metadata.Annotations, "helm.sh/resource-policy", obj.Kind)
		assert.Equal(t, fullname, obj.Metadata.Labels["app.kubernetes.io/managed-by-control-plane"], obj.Kind)
	}
	ks := find(objects, "Kustomization", fullname)
	assert.Equal(t, "./", ks.Spec["path"], "the repository root")
	assert.NotContains(t, find(objects, "GitRepository", fullname).Spec, "secretRef", "anonymous access")
}

func TestGitModeWithoutSyncResourcesRendersNoFluxObject(t *testing.T) {
	objects := render(t, gitMode(
		"engine.kubocd.persistence.git.createSyncResources=false",
		"engine.kubocd.persistence.git.initialize=false",
	)...)

	assert.Empty(t, syncObjects(objects))
	assert.Equal(t, []string{"get", "list", "watch"}, verbs(find(objects, "ClusterRole", fullname).Rules, "kubocd.kubotal.io", "releases"),
		"the server is restricted whoever applies the branch")
	assert.Equal(t, "false", find(objects, "ConfigMap", fullname).Data["ENGINE_KUBOCD_PERSISTENCE_GIT_INITIALIZE"])
}

func TestGitModeWithoutASyncToolRendersNoFluxObject(t *testing.T) {
	for _, set := range [][]string{
		gitMode("engine.kubocd.persistence.git.sync=none"),
		gitMode("engine.kubocd.persistence.git.sync=none", "engine.kubocd.persistence.git.createSyncResources=true"),
	} {
		objects := render(t, set...)

		assert.Empty(t, syncObjects(objects), "nothing for Flux to apply with sync none")
		assert.Nil(t, find(objects, "Role", fullname+"-gitops-sync"), "no Flux sync Role when sync is none")
		assert.Nil(t, find(objects, "RoleBinding", fullname+"-gitops-sync"), "no Flux sync RoleBinding when sync is none")
		assert.Equal(t, "none", find(objects, "ConfigMap", fullname).Data["ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC"])
		assert.Equal(t, []string{"get", "list", "watch"}, verbs(find(objects, "ClusterRole", fullname).Rules, "kubocd.kubotal.io", "releases"))
	}
}

func TestTheRegistryAndGitSecretsShareOneVolumeList(t *testing.T) {
	out, err := helmTemplate(t, gitMode(
		"engine.kubocd.persistence.git.secretName=git-credentials",
		"configuration.registryCredentialsSecret=registry-auth",
	)...)
	require.NoError(t, err, "%s", out)
	objects := decodeAll(t, out)

	podSpec := find(objects, "Deployment", fullname).raw["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	container := podSpec["containers"].([]any)[0].(map[string]any)
	assert.Len(t, container["volumeMounts"], 3)
	assert.Len(t, podSpec["volumes"], 3)
	assert.Equal(t, 1, strings.Count(string(out), "volumeMounts:"))
	assert.Equal(t, 1, strings.Count(string(out), "\n      volumes:"))
}

func TestTheGitSecretIsNotMountedInKubernetesMode(t *testing.T) {
	objects := render(t, "engine.kubocd.persistence.git.secretName=git-credentials", "configuration.registryCredentialsSecret=registry-auth")

	podSpec := find(objects, "Deployment", fullname).raw["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	assert.Equal(t, []any{
		map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "256Mi"}},
		map[string]any{"name": "registry-credentials", "secret": map[string]any{"secretName": "registry-auth", "defaultMode": 288}},
	}, podSpec["volumes"])
}

func TestGitModeNeedsAURL(t *testing.T) {
	for _, args := range [][]string{
		{"engine.kubocd.persistence.mode=git"},
		{"engine.kubocd.persistence.mode=git", "engine.kubocd.persistence.git.sync=none"},
		{"engine.kubocd.persistence.mode=git", "engine.kubocd.persistence.git.createSyncResources=false"},
	} {
		out, err := helmTemplate(t, args...)
		require.Error(t, err, "args %v must fail without git.url", args)
		assert.Contains(t, string(out), "engine.kubocd.persistence.git.url is required in git mode")
	}
}

func TestGitURLRefusesEmbeddedCredentials(t *testing.T) {
	badCreds := []string{
		"https://user:secret@forge.example/okdp/deployments.git",
		"https://user:p/ass@forge.example/okdp/deployments.git",
		"http://token@forge.example/okdp/deployments.git",
		"git://user:pass@forge.example/okdp/deployments.git",
		"git://token@forge.example/okdp/deployments.git",
		"//user:pass@forge.example/okdp/deployments.git",
		"ssh://git:secret@forge.example/okdp/deployments.git",
		"ssh://git:p/ass@forge.example/okdp/deployments.git",
		"ssh://git:@forge.example/okdp/deployments.git",
		"git:secret@forge.example:okdp/deployments.git",
		"git:p/ass@forge.example:okdp/deployments.git",
	}
	for _, u := range badCreds {
		out, err := helmTemplate(t, "engine.kubocd.persistence.mode=git", "engine.kubocd.persistence.git.url="+u)
		require.Error(t, err, "url %q must fail rendering", u)
		assert.Contains(t, string(out), "must not embed", u)
	}
	badQueryOrFragment := []string{
		"https://forge.example/okdp/deployments.git?token=secret",
		"https://user:p?ass@forge.example/okdp/deployments.git",
		"https://user:p#ass@forge.example/okdp/deployments.git",
		"ssh://git@forge.example/okdp/deployments.git#frag",
		"git@forge.example:okdp/deployments.git?token=secret",
	}
	for _, u := range badQueryOrFragment {
		out, err := helmTemplate(t, "engine.kubocd.persistence.mode=git", "engine.kubocd.persistence.git.url="+u)
		require.Error(t, err, "url %q must fail rendering", u)
		assert.Contains(t, string(out), "must not contain", u)
	}
	badSchemes := []string{
		"file:///tmp/repo.git",
		"git://forge.example/okdp/deployments.git",
		"//forge.example/okdp/deployments.git",
		"/srv/repo.git",
	}
	for _, u := range badSchemes {
		out, err := helmTemplate(t, "engine.kubocd.persistence.mode=git", "engine.kubocd.persistence.git.url="+u)
		require.Error(t, err, "url %q must fail rendering", u)
		assert.Contains(t, string(out), "unsupported", u)
	}
	good := []string{
		"https://forge.example/okdp/deployments.git",
		"http://git.git.svc.cluster.local/okdp/okdp-sandbox.git",
		"ssh://git@forge.example/okdp/deployments.git",
		"ssh://git@forge.example:2222/okdp/repo@archive.git",
		"git@forge.example:okdp/deployments.git",
		"git@forge.example:okdp/repo@archive.git",
	}
	for _, u := range good {
		out, err := helmTemplate(t, "engine.kubocd.persistence.mode=git", "engine.kubocd.persistence.git.url="+u)
		require.NoError(t, err, "url %q should render: %s", u, out)
	}
}

func TestLegacyContextConfigurationFallsBackToPlatformContext(t *testing.T) {
	objects := render(t,
		"configuration.contextName=custom-platform",
		"configuration.contextNamespace=custom-ns",
	)
	cm := find(objects, "ConfigMap", fullname)
	require.NotNil(t, cm)
	assert.Equal(t, "control-plane", cm.Data["CONTROL_PLANE_CONTEXT_NAME"])
	assert.Equal(t, "custom-ns", cm.Data["CONTROL_PLANE_CONTEXT_NAMESPACE"])
	assert.Equal(t, "custom-platform", cm.Data["PLATFORM_CONTEXT_NAME"])
	assert.Equal(t, "custom-ns", cm.Data["PLATFORM_CONTEXT_NAMESPACE"])

	explicit := render(t,
		"configuration.contextName=old-platform",
		"configuration.contextNamespace=old-ns",
		"configuration.controlPlaneContextName=cp-prod",
		"configuration.controlPlaneContextNamespace=cp-ns",
		"configuration.platformContextName=new-platform",
		"configuration.platformContextNamespace=plat-ns",
	)
	cm = find(explicit, "ConfigMap", fullname)
	require.NotNil(t, cm)
	assert.Equal(t, "cp-prod", cm.Data["CONTROL_PLANE_CONTEXT_NAME"])
	assert.Equal(t, "cp-ns", cm.Data["CONTROL_PLANE_CONTEXT_NAMESPACE"])
	assert.Equal(t, "new-platform", cm.Data["PLATFORM_CONTEXT_NAME"])
	assert.Equal(t, "plat-ns", cm.Data["PLATFORM_CONTEXT_NAMESPACE"])
}

func TestChartRejectsUnsupportedEngineEnums(t *testing.T) {
	for _, setting := range []string{
		"engine.type=helmfile",
		"engine.kubocd.backend=argocd",
		"engine.kubocd.persistence.mode=s3",
		"engine.kubocd.persistence.git.sync=argocd",
	} {
		out, err := helmTemplate(t, setting)
		require.Error(t, err, "setting %q must fail rendering", setting)
		assert.Contains(t, string(out), "unsupported")
	}
}

func TestGitPathAndBranchNormalization(t *testing.T) {
	for _, tc := range []struct {
		rawPath string
		wantCM  string
		wantKS  string
	}{
		{"./clusters/prod", "clusters/prod", "./clusters/prod"},
		{"/clusters/prod/", "clusters/prod", "./clusters/prod"},
		{"./", "", "./"},
		{"/", "", "./"},
	} {
		objects := render(t, gitMode("engine.kubocd.persistence.git.path="+tc.rawPath, "engine.kubocd.persistence.git.branch=")...)
		cm := find(objects, "ConfigMap", fullname)
		ks := find(objects, "Kustomization", fullname)
		repo := find(objects, "GitRepository", fullname)
		assert.Equal(t, tc.wantCM, cm.Data["ENGINE_KUBOCD_PERSISTENCE_GIT_PATH"], tc.rawPath)
		assert.Equal(t, tc.wantKS, ks.Spec["path"], tc.rawPath)
		assert.Equal(t, "main", cm.Data["ENGINE_KUBOCD_PERSISTENCE_GIT_BRANCH"])
		assert.Equal(t, map[string]any{"branch": "main"}, repo.Spec["ref"])
	}

	for _, tc := range []struct {
		badPath string
		wantErr string
	}{
		{"../escape", ".."},
		{"clusters/../escape", ".."},
		// Helm --set interprets '\' as an escape character, so '\\' passes a
		// literal backslash into .Values.engine.kubocd.persistence.git.path.
		{`clusters\\prod`, "backslash"},
		{".git", ".git"},
		{"./.git", ".git"},
		{"clusters/.GIT/hooks", ".git"},
		{"clusters/.Git", ".git"},
	} {
		out, err := helmTemplate(t, gitMode("engine.kubocd.persistence.git.path="+tc.badPath)...)
		require.Error(t, err, tc.badPath)
		assert.Contains(t, string(out), tc.wantErr, tc.badPath)
	}
}

func TestTheDecoderRefusesADuplicateKey(t *testing.T) {
	var node yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte("spec:\n  volumes: []\n  volumes: []\n"), &node))
	var raw map[string]any
	assert.Error(t, node.Decode(&raw))
}
