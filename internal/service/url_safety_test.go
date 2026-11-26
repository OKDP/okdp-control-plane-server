package service

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/fake"
	"github.com/okdp/okdp-control-plane-server/internal/models"
)

func TestHasEmbeddedURLCredentials(t *testing.T) {
	// Standard userinfo
	assert.True(t, hasEmbeddedURLCredentials("https://user:pass@vault.example.com"))
	assert.True(t, hasEmbeddedURLCredentials("http://token@s3.example.com:9000"))
	assert.True(t, hasEmbeddedURLCredentials("postgresql://admin:secret@db.internal:5432/app"))

	// Compound schemes (JDBC, multi-prefix)
	assert.True(t, hasEmbeddedURLCredentials("jdbc:trino://admin:secret@host:443/catalog"))
	assert.True(t, hasEmbeddedURLCredentials("jdbc:postgresql://admin:secret@db.internal:5432/app"))
	assert.True(t, hasEmbeddedURLCredentials("jdbc:postgresql:thin://admin:secret@db.internal:5432/app"))
	assert.True(t, hasEmbeddedURLCredentials("jdbc:trino://host:443/catalog?password=secret"))
	assert.False(t, hasEmbeddedURLCredentials("jdbc:trino://host:443/catalog?SSL=true"))
	assert.False(t, hasEmbeddedURLCredentials("jdbc:postgresql://db.internal:5432/app?sslmode=require"))

	// Protocol-relative URLs
	assert.True(t, hasEmbeddedURLCredentials("//user:pass@host/path"))
	assert.True(t, hasEmbeddedURLCredentials("//token@host:8080/path"))
	assert.True(t, hasEmbeddedURLCredentials("//host/path?token=abc"))
	assert.False(t, hasEmbeddedURLCredentials("//host/path"))

	// Fail-closed when url.Parse fails and authority contains '@' or split password
	assert.True(t, hasEmbeddedURLCredentials("https://user:s3 cret@host:443/path"))
	assert.True(t, hasEmbeddedURLCredentials("https://user:pass@host:notaport/path"))
	assert.True(t, hasEmbeddedURLCredentials("//user:pass@host:notaport/path"))
	assert.True(t, hasEmbeddedURLCredentials("jdbc:trino://admin:s3 cret@host:443/db"))
	assert.True(t, hasEmbeddedURLCredentials("https://host:notaport/user@example.com"))
	assert.False(t, hasEmbeddedURLCredentials("https://host/user@example.com"))
	assert.False(t, hasEmbeddedURLCredentials("https://host:notaport/path?foo=bar"))

	// Passwords containing '/', '?', or '#'
	for _, u := range []string{
		"https://admin:p/ass@vault.example.com:8200",
		"https://admin:123/secret@vault.example.com:8200",
		"https://admin:p?ass@vault.example.com:8200",
		"https://admin:p#ass@vault.example.com:8200",
		"https://admin:123?secret@vault.example.com:8200",
		"https://admin:123#secret@vault.example.com:8200",
		"//admin:p/ass@vault.example.com:8200",
	} {
		assert.True(t, hasEmbeddedURLCredentials(u), u)
	}

	// '/' preceding '://', excessive unwrap depth (fail-closed), JDBC ';' params, schemeless credentials
	for _, u := range []string{
		"foo/https://user:pass@host",
		"a/b/jdbc:trino://admin:secret@host:443/catalog",
		"a:b:c:d:e:f:g:h:i:j://host/path",
		"jdbc:trino://host:443/catalog;user=admin;password=secret",
		"jdbc:sqlserver://host:1433;password=secret",
		"jdbc:h2:mem:db;password=secret",
		"jdbc:oracle:thin:scott/tiger@//db:1521/orcl",
		"user:SuperSecret@s3.example.com:9000",
		"git:secret@forge.example:org/repo.git",
	} {
		assert.True(t, hasEmbeddedURLCredentials(u), u)
	}

	// Sensitive query parameters (case-insensitive, normalized '_'/'-', & and ; separators)
	for _, u := range []string{
		"https://host/api?password=secret",
		"https://host/api?PASSWORD=secret",
		"https://host/api?foo=1&passwd=secret",
		"https://host/api?pwd=secret",
		"https://host/api?pass=secret",
		"https://host/api?Token=tok123",
		"https://host/api?access_token=tok123",
		"https://host/api?access-token=tok123",
		"https://host/api?auth_token=tok123",
		"https://host/api?private_token=tok123",
		"https://host/api?secret=xyz",
		"https://host/api?Secret_Key=xyz",
		"https://host/api?access_key=ak123",
		"https://host/api?api-key=ak123",
		"https://host/api?client_secret=cs123",
		"https://host/api?private_key=pk123",
		"https://host/api?session-token=st123",
		"https://host/api?sslPassword=sp123",
		"https://host/api?ssl_keystore_password=kp123",
		"https://host/api?sslTrustStorePassword=tp123",
		"https://host/api?X-Amz-Security-Token=aws123",
		"https://host/api?X-Amz-Signature=sig123",
		"https://host/api?a=1;password=secret",
		"https://host/api?%70assword=secret",
		"https://host:notaport/api?token=abc",
	} {
		assert.True(t, hasEmbeddedURLCredentials(u), u)
	}

	// Safe public values
	assert.False(t, hasEmbeddedURLCredentials("https://vault.example.com:8200"))
	assert.False(t, hasEmbeddedURLCredentials("https://vault.example.com:8200?namespace=team-a&ssl=true"))
	assert.False(t, hasEmbeddedURLCredentials("http://storage-s3.default.svc.cluster.local:8333"))
	assert.False(t, hasEmbeddedURLCredentials("adm@okdp.io"))
	assert.False(t, hasEmbeddedURLCredentials("mailto:adm@okdp.io"))
	assert.False(t, hasEmbeddedURLCredentials("owner: adm@okdp.io"))
	assert.False(t, hasEmbeddedURLCredentials("pg-rw.demo.svc.cluster.local"))
	assert.False(t, hasEmbeddedURLCredentials("jdbc:oracle:thin:@//db:1521/orcl"))
	assert.False(t, hasEmbeddedURLCredentials("quay.io/okdp/spark:3.5.0@sha256:abcdef0123456789"))
	assert.False(t, hasEmbeddedURLCredentials("registry.local:5000/okdp/spark@sha256:abcdef0123456789"))
	assert.False(t, hasEmbeddedURLCredentials("git@forge.example:org/repo.git"))
}

func TestSecretStoreRejectsPrivateKeyInCABundle(t *testing.T) {
	pemKey := "-----BEGIN PRIVATE KEY-----\nMIIBVQIBADANBgkqhkiG9w0BAQEFAASCAT8wggE7AgEAAkEA\n-----END PRIVATE KEY-----\n"
	rsaKey := "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBALe\n-----END RSA PRIVATE KEY-----\n"
	certPEM := "-----BEGIN CERTIFICATE-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A\n-----END CERTIFICATE-----\n"

	for _, bad := range []string{pemKey, rsaKey, certPEM + "\n" + pemKey} {
		req := models.SecretStoreRequest{
			Name:     "vault-main",
			Provider: "vault",
			Vault: &models.VaultConfig{
				Server:   "https://vault.example.com:8200",
				Path:     "secret",
				Version:  "v2",
				CABundle: bad,
			},
			Auth: &models.SecretStoreAuth{
				Type:   "kubernetes",
				Config: models.SecretAuthConfig{Role: "okdp"},
			},
		}
		err := validateRequest(req)
		require.Error(t, err)
		assert.True(t, IsValidationError(err))
		assert.Contains(t, err.Error(), "private keys")

		svc := &DefaultSecretStoreService{}
		err = svc.TestConnection(context.Background(), req)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "private keys")
	}

	okReq := models.SecretStoreRequest{
		Name:     "vault-main",
		Provider: "vault",
		Vault: &models.VaultConfig{
			Server:   "https://vault.example.com:8200",
			Path:     "secret",
			Version:  "v2",
			CABundle: certPEM,
		},
		Auth: &models.SecretStoreAuth{
			Type:   "kubernetes",
			Config: models.SecretAuthConfig{Role: "okdp"},
		},
	}
	assert.NoError(t, validateRequest(okReq))
}

func TestDeployAndUpdateRejectEmbeddedCredentialsInParameters(t *testing.T) {
	core, _, _ := newCoreOnFake(t)
	ctx := context.Background()

	_, err := core.DeployService(ctx, "demo", models.ServiceRequest{
		Service:      "jupyterhub",
		InstanceName: "jh",
		Parameters: map[string]any{
			"nested": map[string]any{
				"urls": []any{"https://admin:s3cret@db.internal:5432/app"},
			},
		},
	})
	require.Error(t, err)
	assert.True(t, IsValidationError(err))
	assert.Contains(t, err.Error(), "nested.urls[0]")
	assert.NotContains(t, err.Error(), "s3cret")

	// Deploy cleanly with benign values (including email and OCI image digest).
	_, err = core.DeployService(ctx, "demo", models.ServiceRequest{
		Service:      "jupyterhub",
		InstanceName: "jh",
		Parameters: map[string]any{
			"contact": "adm@okdp.io",
			"image":   "quay.io/okdp/spark:3.5.0@sha256:abcdef0123456789",
		},
	})
	require.NoError(t, err)

	// Update with embedded credentials in parameters must be rejected.
	_, err = core.UpdateServiceParameters(ctx, "demo", "jh", models.ServiceUpdateRequest{
		Parameters: map[string]any{
			"jdbc": "jdbc:trino://host:443/catalog;user=admin;password=secret",
		},
	}, "")
	require.Error(t, err)
	assert.True(t, IsValidationError(err))
	assert.Contains(t, err.Error(), "jdbc")
	assert.NotContains(t, err.Error(), "secret")
}

func TestSecretStoreRejectsVaultServerCredentials(t *testing.T) {
	req := models.SecretStoreRequest{
		Name:     "vault-main",
		Provider: "vault",
		Vault: &models.VaultConfig{
			Server:  "https://user:secret@vault.example.com:8200",
			Path:    "secret",
			Version: "v2",
		},
		Auth: &models.SecretStoreAuth{
			Type:   "kubernetes",
			Config: models.SecretAuthConfig{Role: "okdp"},
		},
	}
	err := validateRequest(req)
	require.Error(t, err)
	assert.True(t, IsValidationError(err))
	assert.Contains(t, err.Error(), "embedded credentials")
}

func TestConnectionRejectsEmbeddedURLCredentials(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t, true)
	req := postgresRequest()
	req.Values["host"] = "postgresql://admin:SuperSecret@db.example.com:5432/analytics"

	_, err := svc.Create(context.Background(), "demo", req)
	require.Error(t, err)
	assert.True(t, IsValidationError(err))
	assert.Contains(t, err.Error(), "embedded credentials")

	_, err = svc.Update(context.Background(), "demo", req.Name, req)
	require.Error(t, err)
	assert.True(t, IsValidationError(err))
	assert.Contains(t, err.Error(), "embedded credentials")
}

// raceDesiredState wraps fake.Engine to simulate a concurrent modification
// between the first GetInstance and UpdateInstance in UpdateServiceParameters.
type raceDesiredState struct {
	*fake.Engine
	mu            sync.Mutex
	conflictOnce  bool
	expectedSeen  []string
	onFirstUpdate func()
}

func (r *raceDesiredState) UpdateInstance(ctx context.Context, in engine.Instance, opts engine.WriteOptions) (engine.Operation, error) {
	r.mu.Lock()
	r.expectedSeen = append(r.expectedSeen, opts.ExpectedRevision)
	trigger := r.conflictOnce
	r.conflictOnce = false
	cb := r.onFirstUpdate
	r.mu.Unlock()
	if trigger {
		if cb != nil {
			cb()
		}
		return engine.Rejected(engine.CodeConflict, "concurrent modification"), nil
	}
	return r.Engine.UpdateInstance(ctx, in, opts)
}

func TestUpdateServiceParametersRetriesOnConcurrentConflictWithoutIfMatch(t *testing.T) {
	fe := fake.New()
	_, err := fe.CreateCatalogEntry(context.Background(), models.PlatformService{
		Name: "jupyterhub", Versions: []string{"1.0.0"}, DefaultVersion: "1.0.0", Category: "Notebooks",
	}, engine.WriteOptions{})
	require.NoError(t, err)

	ref := engine.InstanceRef{Project: "demo", Name: "jh"}
	_, err = fe.CreateInstance(context.Background(), engine.Instance{
		Ref:        ref,
		Service:    "jupyterhub",
		Version:    "1.0.0",
		Parameters: map[string]any{"a": "initial"},
	}, engine.WriteOptions{})
	require.NoError(t, err)

	raced := &raceDesiredState{
		Engine:       fe,
		conflictOnce: true,
		onFirstUpdate: func() {
			_, _ = fe.UpdateInstance(context.Background(), engine.Instance{
				Ref:        ref,
				Service:    "jupyterhub",
				Version:    "1.0.0",
				Parameters: map[string]any{"a": "initial", "b": "from-other-writer"},
			}, engine.WriteOptions{})
		},
	}

	eng := fe.Engine()
	eng.DesiredState = raced
	cluster := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}: "IngressList",
	})
	svc := NewDefaultServiceService(eng, nil, cluster, k8sfake.NewClientset(), nil)

	updated, err := svc.UpdateServiceParameters(context.Background(), "demo", "jh", models.ServiceUpdateRequest{
		Parameters: map[string]any{"a": "mine"},
	}, "")
	require.NoError(t, err)
	assert.Equal(t, "mine", updated.Parameters["a"])
	assert.Equal(t, "from-other-writer", updated.Parameters["b"], "concurrent writer's key must be preserved on CAS retry")
	assert.Len(t, raced.expectedSeen, 2)
	assert.NotEmpty(t, raced.expectedSeen[0], "revision read from GetInstance must be passed to UpdateInstance")
	assert.NotEqual(t, raced.expectedSeen[0], raced.expectedSeen[1])
}
