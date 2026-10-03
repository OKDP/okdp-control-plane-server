package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
	"github.com/sirupsen/logrus"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
)

// PlatformRepository reads the platform configuration: the platform values
// every chart receives under global.okdp (the former KuboCD platform Context,
// same keys), and the console service catalog (platform/catalog.yaml in Git).
type PlatformRepository interface {
	Invalidate()
	// GetPlatformServices returns the managed OKDP services (catalog.yaml categories[].services).
	GetPlatformServices(ctx context.Context) ([]models.PlatformService, error)

	// GetMenuCategories returns the console navigation sections (catalog.yaml categories).
	GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error)

	// GetPackageRepository returns the OCI chart repository prefix (catalog.yaml defaultRepository).
	GetPackageRepository(ctx context.Context) (string, error)

	// GetIngressSuffix returns the ingress domain suffix (global.okdp.ingress.suffix).
	GetIngressSuffix(ctx context.Context) (string, error)

	// GetIdentity returns how OAuth clients are provisioned on this platform
	// (global.okdp.oidc.clientProvisioning).
	GetIdentity(ctx context.Context) (*models.PlatformIdentity, error)

	// GetIdentityOidcConfig returns the OIDC client the console UI should
	// authenticate with (global.okdp.identity.oidc, nil when the platform
	// values do not publish one).
	GetIdentityOidcConfig(ctx context.Context) (*models.IdentityOidcConfig, error)

	// GetOidcIssuer returns the issuer whose tokens the API accepts, "" when the platform names none.
	GetOidcIssuer(ctx context.Context) (string, error)

	// GetOidcClientID returns the client a token must be issued for, "" when the platform names none.
	GetOidcClientID(ctx context.Context) (string, error)

	// GetOidcInsecureSkipVerify reports whether the issuer's certificate is taken on trust (global.okdp.oidc.insecureSkipVerify).
	GetOidcInsecureSkipVerify(ctx context.Context) (bool, error)

	// GetIdentityProvisioningProvider returns the OIDC client provisioning backend
	// (global.okdp.identity.provisioning.provider, "" when unset, meaning none).
	GetIdentityProvisioningProvider(ctx context.Context) (string, error)

	// GetKeycloakProvisioningConfig returns the Keycloak adapter configuration
	// (global.okdp.identity.provisioning.keycloak).
	GetKeycloakProvisioningConfig(ctx context.Context) (*provisioning.KeycloakConfig, error)

	// GetProfileImages returns available images per profile type from global.okdp.jupyter.profiles.
	GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error)

	// GetSparkConfig returns Spark operator configuration from global.okdp.sparkOperator.
	GetSparkConfig(ctx context.Context) (*models.SparkConfig, error)
}

// platformValuesCacheTTL bounds how stale the platform values may be. They
// change rarely, and every page reads several of them.
const platformValuesCacheTTL = 5 * time.Second

// platformRepository reads the platform values from the ConfigMap Flux layers
// into every release (okdp-platform-values, key values.yaml), falling back to
// platform/platform-values.yaml in Git when there is no such ConfigMap (Argo CD
// reads the file straight from Git and creates none). The catalog always comes
// from Git.
type platformRepository struct {
	client      kubernetes.Interface
	namespace   string
	deployments *gitops.Deployments

	mu       sync.Mutex
	cached   map[string]interface{}
	cachedAt time.Time
}

// NewPlatformRepository reads the platform values ConfigMap in namespace.
func NewPlatformRepository(client kubernetes.Interface, namespace string, deployments *gitops.Deployments) PlatformRepository {
	return &platformRepository{
		client:      client,
		namespace:   namespace,
		deployments: deployments,
	}
}

// catalogCategories returns the ordered sections of the catalog, each with its
// raw services. A service belongs to the section it is nested in.
func catalogCategories(v map[string]interface{}) ([]map[string]interface{}, bool, error) {
	raw, found, err := unstructured.NestedSlice(v, "categories")
	if err != nil {
		return nil, false, fmt.Errorf("failed to read categories from %s: %w", gitops.CatalogPath, err)
	}
	if !found {
		return nil, false, nil
	}
	var cats []map[string]interface{}
	for _, r := range raw {
		if m, ok := r.(map[string]interface{}); ok {
			cats = append(cats, m)
		}
	}
	return cats, true, nil
}

func platformServiceFromMap(m map[string]interface{}, category string) models.PlatformService {
	defaultVersion := getString(m, "default")
	if defaultVersion == "" {
		defaultVersion = getString(m, "tag")
	}
	svc := models.PlatformService{
		Name:           getString(m, "name"),
		DefaultVersion: defaultVersion,
		Description:    getString(m, "description"),
		Category:       category,
		Repository:     getString(m, "repository"),
		Label:          getString(m, "label"),
		ExposesUI:      getBoolPtr(m, "exposesUI"),
	}
	if rawVersions, ok := m["versions"].([]interface{}); ok {
		for _, v := range rawVersions {
			if s, ok := v.(string); ok {
				svc.Versions = append(svc.Versions, s)
			}
		}
	}
	if len(svc.Versions) == 0 && svc.DefaultVersion != "" {
		svc.Versions = []string{svc.DefaultVersion}
	}
	return svc
}

func (r *platformRepository) GetPlatformServices(ctx context.Context) ([]models.PlatformService, error) {
	u, err := r.catalog(ctx)
	if err != nil {
		return nil, err
	}

	cats, found, err := catalogCategories(u)
	if err != nil {
		return nil, err
	}
	if !found {
		logrus.Warn("No categories found in " + gitops.CatalogPath)
		return []models.PlatformService{}, nil
	}

	// Never nil: an empty catalog answers [] in JSON, not null.
	services := make([]models.PlatformService, 0)
	for _, cat := range cats {
		title := getString(cat, "title")
		rawServices, _ := cat["services"].([]interface{})
		for _, raw := range rawServices {
			m, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			services = append(services, platformServiceFromMap(m, title))
		}
	}
	return services, nil
}

func (r *platformRepository) GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error) {
	u, err := r.catalog(ctx)
	if err != nil {
		return nil, err
	}

	cats, found, err := catalogCategories(u)
	if err != nil {
		return nil, err
	}
	if !found {
		return []models.MenuCategory{}, nil
	}

	categories := make([]models.MenuCategory, 0, len(cats))
	for i, cat := range cats {
		title := getString(cat, "title")
		categories = append(categories, models.MenuCategory{
			Key:   title,
			Label: title,
			Icon:  getString(cat, "icon"),
			Order: i + 1,
		})
	}
	return categories, nil
}

func (r *platformRepository) GetPackageRepository(ctx context.Context) (string, error) {
	u, err := r.catalog(ctx)
	if err != nil {
		return "", err
	}
	repo := getString(u, "defaultRepository")
	if repo == "" {
		return "", fmt.Errorf("defaultRepository not found in %s", gitops.CatalogPath)
	}
	return strings.TrimPrefix(repo, "oci://"), nil
}

func (r *platformRepository) GetIngressSuffix(ctx context.Context) (string, error) {
	v, err := r.values(ctx)
	if err != nil {
		return "", err
	}
	suffix, _, _ := unstructured.NestedString(v, "ingress", "suffix")
	if suffix == "" {
		return "", fmt.Errorf("ingress.suffix not found in the platform values (global.okdp)")
	}
	return suffix, nil
}

// GetIdentity reads oidc.clientProvisioning. An absent block means the platform
// was never told, and the safe reading is that the clients already exist in
// Keycloak (existing).
func (r *platformRepository) GetIdentity(ctx context.Context) (*models.PlatformIdentity, error) {
	v, err := r.values(ctx)
	if err != nil {
		return nil, err
	}
	provisioning, _, _ := unstructured.NestedString(v, "oidc", "clientProvisioning")
	if provisioning == "" {
		// Legacy path, kept while platform values migrate to the single oidc block.
		provisioning, _, _ = unstructured.NestedString(v, "identity", "clientProvisioning")
	}
	identity := &models.PlatformIdentity{ClientProvisioning: provisioning}
	if identity.ClientProvisioning == "" {
		identity.ClientProvisioning = models.ClientProvisioningExisting
	}
	return identity, identity.Validate()
}

func (r *platformRepository) GetIdentityOidcConfig(ctx context.Context) (*models.IdentityOidcConfig, error) {
	v, err := r.values(ctx)
	if err != nil {
		return nil, err
	}
	authority, _, _ := unstructured.NestedString(v, "identity", "oidc", "authority")
	clientID, _, _ := unstructured.NestedString(v, "identity", "oidc", "clientId")
	if authority == "" || clientID == "" {
		return nil, nil
	}
	scope, _, _ := unstructured.NestedString(v, "identity", "oidc", "scope")
	return &models.IdentityOidcConfig{
		Authority: authority,
		ClientID:  clientID,
		Scope:     scope,
	}, nil
}

// GetOidcIssuer reads the issuer from the platform values, so a platform declaring its
// identity provider does not repeat it in the server's environment.
func (r *platformRepository) GetOidcIssuer(ctx context.Context) (string, error) {
	v, err := r.values(ctx)
	if err != nil {
		return "", err
	}
	if authority, _, _ := unstructured.NestedString(v, "identity", "oidc", "authority"); authority != "" {
		return authority, nil
	}
	issuer, _, _ := unstructured.NestedString(v, "oidc", "issuerUri")
	return issuer, nil
}

// GetOidcClientID mirrors GetOidcIssuer's two layouts.
func (r *platformRepository) GetOidcClientID(ctx context.Context) (string, error) {
	v, err := r.values(ctx)
	if err != nil {
		return "", err
	}
	if clientID, _, _ := unstructured.NestedString(v, "oidc", "clientId"); clientID != "" {
		return clientID, nil
	}
	clientID, _, _ := unstructured.NestedString(v, "identity", "oidc", "clientId")
	return clientID, nil
}

// GetOidcInsecureSkipVerify reads the platform's answer to a self-signed issuer
// certificate, the setting the Keycloak provisioner already honors.
func (r *platformRepository) GetOidcInsecureSkipVerify(ctx context.Context) (bool, error) {
	v, err := r.values(ctx)
	if err != nil {
		return false, err
	}
	insecure, _, _ := unstructured.NestedBool(v, "oidc", "insecureSkipVerify")
	return insecure, nil
}

// GetIdentityProvisioningProvider names the backend that unmakes the OAuth
// clients (global.okdp.identity.provisioning.provider). Unset means the platform
// provisions no client of its own: with oidc.clientProvisioning existing or dcr,
// the clients are made in Keycloak beforehand or by the packages themselves.
func (r *platformRepository) GetIdentityProvisioningProvider(ctx context.Context) (string, error) {
	v, err := r.values(ctx)
	if err != nil {
		return "", err
	}
	provider, _, _ := unstructured.NestedString(v, "identity", "provisioning", "provider")
	if provider != "" {
		return provider, nil
	}
	return provisioning.ProviderNone, nil
}

func (r *platformRepository) GetKeycloakProvisioningConfig(ctx context.Context) (*provisioning.KeycloakConfig, error) {
	v, err := r.values(ctx)
	if err != nil {
		return nil, err
	}

	issuer, _, _ := unstructured.NestedString(v, "identity", "provisioning", "keycloak", "issuerUri")
	if issuer == "" {
		return nil, fmt.Errorf("identity.provisioning.keycloak.issuerUri not found in the platform values (global.okdp)")
	}

	secretPath := []string{"identity", "provisioning", "keycloak", "credentialsSecret"}
	secretName, _, _ := unstructured.NestedString(v, append(secretPath, "name")...)
	if secretName == "" {
		return nil, fmt.Errorf("identity.provisioning.keycloak.credentialsSecret.name not found in the platform values (global.okdp)")
	}
	secretNamespace, _, _ := unstructured.NestedString(v, append(secretPath, "namespace")...)
	clientIDKey, _, _ := unstructured.NestedString(v, append(secretPath, "clientIdKey")...)
	if clientIDKey == "" {
		clientIDKey = "client_id"
	}
	clientSecretKey, _, _ := unstructured.NestedString(v, append(secretPath, "clientSecretKey")...)
	if clientSecretKey == "" {
		clientSecretKey = "client_secret"
	}
	insecure, _, _ := unstructured.NestedBool(v, "identity", "provisioning", "keycloak", "insecureSkipTLSVerify")

	return &provisioning.KeycloakConfig{
		IssuerURI: issuer,
		CredentialsSecret: provisioning.KeycloakSecretRef{
			Namespace:       secretNamespace,
			Name:            secretName,
			ClientIDKey:     clientIDKey,
			ClientSecretKey: clientSecretKey,
		},
		InsecureSkipTLSVerify: insecure,
	}, nil
}

func (r *platformRepository) GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error) {
	v, err := r.values(ctx)
	if err != nil {
		return nil, err
	}

	result := make(map[string][]models.ProfileImage)
	profileTypes := []string{"jupyterlab", "vscode", "rstudio"}

	for _, pType := range profileTypes {
		rawImages, found, err := unstructured.NestedSlice(v, "jupyter", "profiles", pType, "images")
		if err != nil || !found {
			continue
		}

		var images []models.ProfileImage
		for _, raw := range rawImages {
			m, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			images = append(images, models.ProfileImage{
				Label: getString(m, "label"),
				Image: getString(m, "image"),
			})
		}
		if len(images) > 0 {
			result[pType] = images
		}
	}
	return result, nil
}

func (r *platformRepository) GetSparkConfig(ctx context.Context) (*models.SparkConfig, error) {
	v, err := r.values(ctx)
	if err != nil {
		return nil, err
	}

	sparkOp, found, err := unstructured.NestedMap(v, "sparkOperator")
	if err != nil {
		return nil, fmt.Errorf("failed to read sparkOperator from the platform values: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("sparkOperator not found in the platform values (global.okdp)")
	}

	cfg := &models.SparkConfig{}

	if imgMap, ok := sparkOp["image"].(map[string]interface{}); ok {
		cfg.Image = models.SparkConfigImage{
			Registry:   getString(imgMap, "registry"),
			Repository: getString(imgMap, "repository"),
			Tag:        getString(imgMap, "tag"),
		}
	}

	if sparkMap, ok := sparkOp["spark"].(map[string]interface{}); ok {
		cfg.Spark.DefaultVersion = getString(sparkMap, "defaultVersion")

		if rawImages, ok := sparkMap["images"].([]interface{}); ok {
			for _, raw := range rawImages {
				m, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				cfg.Spark.Images = append(cfg.Spark.Images, models.SparkImage{
					Label: getString(m, "label"),
					Image: getString(m, "image"),
				})
			}
		}

		if defaultsMap, ok := sparkMap["defaults"].(map[string]interface{}); ok {
			if driverMap, ok := defaultsMap["driver"].(map[string]interface{}); ok {
				cfg.Spark.Defaults.Driver = models.ResourceDefaults{
					Cores:  getInt(driverMap, "cores"),
					Memory: getString(driverMap, "memory"),
				}
			}
			if execMap, ok := defaultsMap["executor"].(map[string]interface{}); ok {
				cfg.Spark.Defaults.Executor = models.ExecutorDefaults{
					ResourceDefaults: models.ResourceDefaults{
						Cores:  getInt(execMap, "cores"),
						Memory: getString(execMap, "memory"),
					},
					Instances: getInt(execMap, "instances"),
				}
			}
		}
	}

	return cfg, nil
}

// values returns the global.okdp map of the platform values.
func (r *platformRepository) values(ctx context.Context) (map[string]interface{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cached != nil && time.Since(r.cachedAt) < platformValuesCacheTTL {
		return r.cached, nil
	}

	raw, source, err := r.readPlatformValues(ctx)
	if err != nil {
		return nil, err
	}
	doc, err := gitops.DecodeValues(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	okdp, found, err := unstructured.NestedMap(doc, "global", "okdp")
	if err != nil || !found {
		return nil, fmt.Errorf("%s carries no global.okdp", source)
	}
	r.cached = okdp
	r.cachedAt = time.Now()
	return okdp, nil
}

func (r *platformRepository) readPlatformValues(ctx context.Context) ([]byte, string, error) {
	if r.client != nil {
		cm, err := r.client.CoreV1().ConfigMaps(r.namespace).Get(ctx, gitops.PlatformValuesConfigMap, metav1.GetOptions{})
		switch {
		case err == nil:
			source := fmt.Sprintf("ConfigMap %s/%s", r.namespace, gitops.PlatformValuesConfigMap)
			data, ok := cm.Data[gitops.ValuesKey]
			if !ok {
				return nil, "", fmt.Errorf("%s has no %s key", source, gitops.ValuesKey)
			}
			return []byte(data), source, nil
		case apierrors.IsNotFound(err):
			logrus.Debugf("No ConfigMap %s/%s, reading the platform values from Git", r.namespace, gitops.PlatformValuesConfigMap)
		default:
			// Forbidden or unreachable: Git still answers.
			logrus.WithError(err).Warn("Could not read the platform values ConfigMap, reading them from Git")
		}
	}
	if r.deployments == nil {
		return nil, "", fmt.Errorf("no platform values: ConfigMap %s/%s not found", r.namespace, gitops.PlatformValuesConfigMap)
	}
	raw, err := r.deployments.ReadPlatformValues(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("no platform values: ConfigMap %s/%s not found, and %w", r.namespace, gitops.PlatformValuesConfigMap, err)
	}
	return raw, gitops.PlatformValuesPath, nil
}

// catalog returns platform/catalog.yaml. It is the former serviceCatalog
// block; a file that keeps the serviceCatalog wrapper is read as well.
func (r *platformRepository) catalog(ctx context.Context) (map[string]interface{}, error) {
	if r.deployments == nil {
		return nil, errors.New("no deployments repository configured")
	}
	doc, err := r.deployments.ReadCatalog(ctx)
	if err != nil {
		if errors.Is(err, gitops.ErrNotFound) {
			return map[string]interface{}{}, nil
		}
		return nil, err
	}
	return unwrapCatalog(doc), nil
}

// unwrapCatalog returns the catalog body, with or without the serviceCatalog
// wrapper.
func unwrapCatalog(doc map[string]interface{}) map[string]interface{} {
	if inner, ok := doc["serviceCatalog"].(map[string]interface{}); ok {
		return inner
	}
	return doc
}

func (r *platformRepository) Invalidate() {
	r.mu.Lock()
	r.cached = nil
	r.mu.Unlock()
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getBoolPtr(m map[string]interface{}, key string) *bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return &b
		}
	}
	return nil
}

func getInt(m map[string]interface{}, key string) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case int64:
			return int(n)
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return 0
}
