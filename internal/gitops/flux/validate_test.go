package flux

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestNewRejectsAnIncompleteConfig(t *testing.T) {
	_, err := New(newClient(), Config{Namespace: testNS})
	require.Error(t, err)
	for _, field := range []string{"GitRepositoryName", "KustomizationName", "ExpectedRepository.URL", "ExpectedRepository.Branch"} {
		assert.ErrorContains(t, err, field)
	}

	_, err = New(nil, testConfig())
	assert.ErrorContains(t, err, "dynamic client is nil")
}

func TestValidateAcceptsEquivalentURLAndPathForms(t *testing.T) {
	cases := []struct {
		name               string
		fluxURL, fluxPath  string
		serverURL, srvPath string
	}{
		{"git suffix and slash", "https://git.example.com/platform/state.git/", "./clusters/prod/", "https://git.example.com/platform/state", "clusters/prod"},
		{"host case and default port", "https://GIT.Example.com:443/platform/state", "clusters/prod", "https://git.example.com/platform/state.git", "clusters/prod"},
		{"scp form on the server", "ssh://git@git.example.com/platform/state.git", "clusters/prod", "git@git.example.com:platform/state.git", "clusters/prod"},
		{"root as dot", "https://git.example.com/platform/state", ".", "https://git.example.com/platform/state", ""},
		{"root as dot slash", "https://git.example.com/platform/state", "./", "https://git.example.com/platform/state", ""},
		{"root as empty", "https://git.example.com/platform/state", "", "https://git.example.com/platform/state", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := set(t, gitRepository(shaA), tc.fluxURL, "spec", "url")
			ks := set(t, kustomization(shaA), tc.fluxPath, "spec", "path")
			cfg := testConfig()
			cfg.ExpectedRepository.URL = tc.serverURL
			cfg.ExpectedRepository.Path = tc.srvPath
			a, err := New(newClient(repo, ks), cfg)
			require.NoError(t, err)
			assert.NoError(t, a.Validate(context.Background()))
		})
	}
}

func TestValidateReportsAMissingObject(t *testing.T) {
	err := newAdapter(t, newClient(kustomization(shaA))).Validate(context.Background())
	assert.True(t, apierrors.IsNotFound(err))
	assert.ErrorContains(t, err, "GitRepository okdp/okdp-state")

	err = newAdapter(t, newClient(gitRepository(shaA))).Validate(context.Background())
	assert.True(t, apierrors.IsNotFound(err))
	assert.ErrorContains(t, err, "Kustomization okdp/okdp-state")
}

func TestValidateReportsMismatches(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, repo, ks *unstructured.Unstructured)
		want   string
	}{
		{"source name", func(t *testing.T, _, ks *unstructured.Unstructured) {
			set(t, ks, "other", "spec", "sourceRef", "name")
		}, "spec.sourceRef GitRepository okdp/other"},
		{"source kind", func(t *testing.T, _, ks *unstructured.Unstructured) {
			set(t, ks, "OCIRepository", "spec", "sourceRef", "kind")
		}, "spec.sourceRef OCIRepository okdp/okdp-state"},
		{"source namespace", func(t *testing.T, _, ks *unstructured.Unstructured) {
			set(t, ks, "flux-system", "spec", "sourceRef", "namespace")
		}, "spec.sourceRef GitRepository flux-system/okdp-state"},
		{"url", func(t *testing.T, repo, _ *unstructured.Unstructured) {
			set(t, repo, "https://git.example.com/platform/other", "spec", "url")
		}, "pulls https://git.example.com/platform/other"},
		{"url path case", func(t *testing.T, repo, _ *unstructured.Unstructured) {
			set(t, repo, "https://git.example.com/Platform/State", "spec", "url")
		}, "pulls https://git.example.com/Platform/State"},
		{"branch", func(t *testing.T, repo, _ *unstructured.Unstructured) {
			set(t, repo, "develop", "spec", "ref", "branch")
		}, `follows branch "develop"`},
		{"no branch", func(t *testing.T, repo, _ *unstructured.Unstructured) {
			unstructured.RemoveNestedField(repo.Object, "spec", "ref")
		}, "has no spec.ref.branch"},
		{"tag", func(t *testing.T, repo, _ *unstructured.Unstructured) {
			set(t, repo, "v1.0.0", "spec", "ref", "tag")
		}, `pins spec.ref.tag "v1.0.0"`},
		{"commit", func(t *testing.T, repo, _ *unstructured.Unstructured) {
			set(t, repo, shaB, "spec", "ref", "commit")
		}, `pins spec.ref.commit "` + shaB + `"`},
		{"semver", func(t *testing.T, repo, _ *unstructured.Unstructured) {
			set(t, repo, ">=1.0.0", "spec", "ref", "semver")
		}, `pins spec.ref.semver`},
		{"path", func(t *testing.T, _, ks *unstructured.Unstructured) {
			set(t, ks, "./clusters/staging", "spec", "path")
		}, `applies spec.path "./clusters/staging"`},
		{"path with a trailing space", func(t *testing.T, _, ks *unstructured.Unstructured) {
			set(t, ks, "clusters/prod ", "spec", "path")
		}, `applies spec.path "clusters/prod "`},
		{"no service account", func(t *testing.T, _, ks *unstructured.Unstructured) {
			unstructured.RemoveNestedField(ks.Object, "spec", "serviceAccountName")
		}, "has no spec.serviceAccountName"},
		{"empty service account", func(t *testing.T, _, ks *unstructured.Unstructured) {
			set(t, ks, "", "spec", "serviceAccountName")
		}, "has no spec.serviceAccountName"},
		{"prune false", func(t *testing.T, _, ks *unstructured.Unstructured) {
			set(t, ks, false, "spec", "prune")
		}, "spec.prune=false"},
		{"prune absent", func(t *testing.T, _, ks *unstructured.Unstructured) {
			unstructured.RemoveNestedField(ks.Object, "spec", "prune")
		}, "spec.prune=false"},
		{"targetNamespace set", func(t *testing.T, _, ks *unstructured.Unstructured) {
			set(t, ks, "other-ns", "spec", "targetNamespace")
		}, `sets spec.targetNamespace "other-ns"`},
		{"kubeConfig set", func(t *testing.T, _, ks *unstructured.Unstructured) {
			set(t, ks, map[string]any{"secretRef": map[string]any{"name": "remote"}}, "spec", "kubeConfig")
		}, "sets spec.kubeConfig"},
		{"path traversal segment", func(t *testing.T, _, ks *unstructured.Unstructured) {
			set(t, ks, "clusters/staging/../prod", "spec", "path")
		}, `containing a ".." segment`},
		{"leading path traversal", func(t *testing.T, _, ks *unstructured.Unstructured) {
			set(t, ks, "../clusters/prod", "spec", "path")
		}, `containing a ".." segment`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, ks := gitRepository(shaA), kustomization(shaA)
			tc.mutate(t, repo, ks)
			err := newAdapter(t, newClient(repo, ks)).Validate(context.Background())
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestValidateReportsEveryMismatchAtOnce(t *testing.T) {
	repo := set(t, gitRepository(shaA), "develop", "spec", "ref", "branch")
	ks := set(t, kustomization(shaA), "elsewhere", "spec", "path")
	err := newAdapter(t, newClient(repo, ks)).Validate(context.Background())
	assert.ErrorContains(t, err, "develop")
	assert.ErrorContains(t, err, "elsewhere")
}

func TestValidateNeverQuotesURLCredentials(t *testing.T) {
	repo := set(t, gitRepository(shaA), "https://user:s3cret@git.example.com/platform/other", "spec", "url")
	err := newAdapter(t, newClient(repo, kustomization(shaA))).Validate(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "s3cret")
	assert.NotContains(t, err.Error(), "user")
}

func TestValidateRejectsEmbeddedCredentialsAndQueryStringsEvenWhenRepoMatches(t *testing.T) {
	cases := []struct {
		name      string
		gotURL    string
		serverURL string
		wantErr   string
	}{
		{"https user and password", "https://user:s3cret@git.example.com/platform/state.git", "https://git.example.com/platform/state.git", "must not embed"},
		{"https user only", "https://user@git.example.com/platform/state.git", "https://git.example.com/platform/state.git", "must not embed"},
		{"http user only", "http://user@git.example.com/platform/state.git", "http://git.example.com/platform/state.git", "must not embed"},
		{"ssh password", "ssh://git:s3cret@git.example.com/platform/state.git", "ssh://git@git.example.com/platform/state.git", "must not embed"},
		{"ssh empty password", "ssh://git:@git.example.com/platform/state.git", "ssh://git@git.example.com/platform/state.git", "must not embed"},
		{"scp password", "git:s3cret@git.example.com:platform/state.git", "git@git.example.com:platform/state.git", "must not embed"},
		{"https query string", "https://git.example.com/platform/state.git?token=s3cret", "https://git.example.com/platform/state.git", "query string"},
		{"ssh query string", "ssh://git@git.example.com/platform/state.git?token=s3cret", "ssh://git@git.example.com/platform/state.git", "query string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := set(t, gitRepository(shaA), tc.gotURL, "spec", "url")
			cfg := testConfig()
			cfg.ExpectedRepository.URL = tc.serverURL
			a, err := New(newClient(repo, kustomization(shaA)), cfg)
			require.NoError(t, err)
			err = a.Validate(context.Background())
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.wantErr)
			assert.NotContains(t, err.Error(), "s3cret")
			assert.NotContains(t, err.Error(), "user")
		})
	}
}

func TestValidateNeverQuotesCredentialsOfAnUnparsableURL(t *testing.T) {
	for _, raw := range []string{
		"https://user:s3 cret@git.example.com/platform/state",
		"https://user:s3/cret@git.example.com/platform/state",
		"https://user:s3#cret@git.example.com/platform/state",
		"https://user:s3?cret@git.example.com/platform/state",
		"user:s3/cret@git.example.com:platform/state",
		"https://user/s3cret@git.example.com/platform/state",
		"user:s3?cret@git.example.com:platform/state",
		"https://user?s3cret@git.example.com/platform/state",
		"https://user:12/s3cret@git.example.com/platform/state",
	} {
		t.Run("GitRepository "+raw, func(t *testing.T) {
			repo := set(t, gitRepository(shaA), raw, "spec", "url")
			err := newAdapter(t, newClient(repo, kustomization(shaA))).Validate(context.Background())
			require.Error(t, err)
			assertNoCredentials(t, err)
		})
		t.Run("ExpectedRepository "+raw, func(t *testing.T) {
			cfg := testConfig()
			cfg.ExpectedRepository.URL = raw
			a, err := New(newClient(gitRepository(shaA), kustomization(shaA)), cfg)
			require.NoError(t, err)
			err = a.Validate(context.Background())
			require.Error(t, err)
			assertNoCredentials(t, err)
		})
	}
}

func assertNoCredentials(t *testing.T, err error) {
	t.Helper()
	for _, secret := range []string{"user", "s3", "cret"} {
		assert.NotContains(t, err.Error(), secret)
	}
}

func TestValidateAcceptsAnAtSignInTheRepositoryPath(t *testing.T) {
	for _, raw := range []string{
		"ssh://git@git.example.com/team/repo@archive.git",
		"git@git.example.com:team/repo@archive.git",
		"https://git.example.com/team/repo%40archive",
	} {
		t.Run(raw, func(t *testing.T) {
			repo := set(t, gitRepository(shaA), raw, "spec", "url")
			cfg := testConfig()
			cfg.ExpectedRepository.URL = raw
			a, err := New(newClient(repo, kustomization(shaA)), cfg)
			require.NoError(t, err)
			assert.NoError(t, a.Validate(context.Background()))
		})
	}
}

func TestValidateQuotesAURLWhoseUserInfoParsed(t *testing.T) {
	repo := set(t, gitRepository(shaA), "ssh://git@git.example.com/platform/other.git", "spec", "url")
	cfg := testConfig()
	cfg.ExpectedRepository.URL = "git@git.example.com:platform/state.git"
	a, err := New(newClient(repo, kustomization(shaA)), cfg)
	require.NoError(t, err)
	assert.ErrorContains(t, a.Validate(context.Background()),
		"pulls ssh://git.example.com/platform/other, the server commits to ssh://git.example.com/platform/state")
}

func TestNormalizePath(t *testing.T) {
	for in, want := range map[string]string{
		"": "", ".": "", "./": "", "/": "", "apps": "apps", "./apps": "apps",
		"apps/": "apps", "/apps/prod/": "apps/prod", "./apps/./prod": "apps/prod",
	} {
		assert.Equal(t, want, normalizePath(in), in)
	}
}
