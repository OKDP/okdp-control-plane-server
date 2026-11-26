package repository

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
	"github.com/sirupsen/logrus"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var contextGVR = schema.GroupVersionResource{
	Group:    "kubocd.kubotal.io",
	Version:  "v1alpha1",
	Resource: "contexts",
}

// ContextRepository reads KuboCD Context CRs to extract platform configuration.
type ContextRepository interface {
	Invalidate()
	// GetPlatformServices returns the managed OKDP services (from spec.context.serviceCatalog.services).
	GetPlatformServices(ctx context.Context) ([]models.PlatformService, error)

	// GetMenuCategories returns the console navigation sections (from spec.context.serviceCatalog.categories).
	GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error)

	// GetPackageRepository returns the OCI package repository prefix (from spec.context.serviceCatalog.defaultRepository).
	GetPackageRepository(ctx context.Context) (string, error)

	// GetIngressSuffix returns the ingress domain suffix (from spec.context.ingress.suffix).
	GetIngressSuffix(ctx context.Context) (string, error)

	// GetIdentity returns how OAuth clients are provisioned on this platform
	// (from spec.context.platform.identity).
	GetIdentity(ctx context.Context) (*models.PlatformIdentity, error)

	// GetKubauthNamespace returns the namespace kubauth resources live in. It is
	// only meaningful when clients are provisioned by kubauth.
	GetKubauthNamespace(ctx context.Context) (string, error)

	// GetIdentityProvider returns the identity provider the platform is wired to
	// (from spec.context.identity.provider, "" when unset, meaning external).
	// The kubauth-specific Identity API is only exposed when it is "kubauth".
	GetIdentityProvider(ctx context.Context) (string, error)

	// GetIdentityOidcConfig returns the OIDC client the console UI should
	// authenticate with (from spec.context.identity.oidc, nil when the
	// Context does not publish one).
	GetIdentityOidcConfig(ctx context.Context) (*models.IdentityOidcConfig, error)

	// GetOidcIssuer returns the issuer whose tokens the API accepts, "" when the Context names none.
	GetOidcIssuer(ctx context.Context) (string, error)

	// GetOidcClientID returns the client a token must be issued for, "" when the Context names none.
	GetOidcClientID(ctx context.Context) (string, error)

	// GetOidcInsecureSkipVerify reports whether the issuer's certificate is taken on trust (from spec.context.oidc.insecureSkipVerify).
	GetOidcInsecureSkipVerify(ctx context.Context) (bool, error)

	// GetIdentityProvisioningProvider returns the OIDC client provisioning backend
	// (from spec.context.identity.provisioning.provider, "" when unset, meaning none).
	GetIdentityProvisioningProvider(ctx context.Context) (string, error)

	// GetKeycloakProvisioningConfig returns the Keycloak adapter configuration
	// (from spec.context.identity.provisioning.keycloak).
	GetKeycloakProvisioningConfig(ctx context.Context) (*provisioning.KeycloakConfig, error)

	// GetProfileImages returns available images per profile type from spec.context.jupyter.profiles.
	GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error)

	// GetSparkConfig returns Spark operator configuration from spec.context.sparkOperator.
	GetSparkConfig(ctx context.Context) (*models.SparkConfig, error)
}

// ContextRef names a KuboCD Context.
type ContextRef struct {
	Name      string
	Namespace string
}

func (r ContextRef) String() string { return r.Namespace + "/" + r.Name }

// k8sContextRepository reads two Contexts, because they hold two different
// kinds of configuration.
//
// The Control Plane Context holds what only the console reads: the service
// catalog, the categories of the portal, the package repository. No Release
// references it, so editing the catalog reconciles nothing.
//
// The platform Context holds what a package template reads: the ingress suffix,
// the OIDC and identity settings, the profile images, the Spark settings.
// Every Release merges it, so it changes rarely and a change is expensive.
//
// The server reads from both. It consumes platform data, it does not own it.
// The catalog is read from the Control Plane Context, and from the platform
// one while the Control Plane Context does not exist or carries no
// serviceCatalog, which is what a cluster with a single Context looks like.
const contextCacheTTL = 5 * time.Second

type k8sContextRepository struct {
	client       dynamic.Interface
	platform     ContextRef
	controlPlane ContextRef

	mu    sync.Mutex
	cache map[ContextRef]cachedContext
}

// cachedContext is a Context as last read, nil when it did not exist.
type cachedContext struct {
	object   *unstructured.Unstructured
	cachedAt time.Time
}

// NewContextRepository reads everything from one Context.
func NewContextRepository(client dynamic.Interface, name, namespace string) ContextRepository {
	ref := ContextRef{Name: name, Namespace: namespace}
	return NewSplitContextRepository(client, ref, ref)
}

// NewSplitContextRepository reads the platform settings from platform and the
// catalog from controlPlane, falling back to platform for the catalog. Two
// equal refs are one Context holding everything.
func NewSplitContextRepository(client dynamic.Interface, platform, controlPlane ContextRef) ContextRepository {
	return &k8sContextRepository{
		client:       client,
		platform:     platform,
		controlPlane: controlPlane,
		cache:        map[ContextRef]cachedContext{},
	}
}

// catalogCategories returns the ordered sections of the Context catalog, each
// with its raw services. The catalog lives under
// spec.context.serviceCatalog.categories[], a service belongs to the section
// it is nested in.
func catalogCategories(u *unstructured.Unstructured) ([]map[string]interface{}, bool, error) {
	raw, found, err := unstructured.NestedSlice(u.Object, "spec", "context", "serviceCatalog", "categories")
	if err != nil {
		return nil, false, fmt.Errorf("failed to read serviceCatalog.categories from Context: %w", err)
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

func (r *k8sContextRepository) GetPlatformServices(ctx context.Context) ([]models.PlatformService, error) {
	u, err := r.catalogContext(ctx)
	if err != nil {
		return nil, err
	}
	return ContextPlatformServices(u)
}

func (r *k8sContextRepository) GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error) {
	u, err := r.catalogContext(ctx)
	if err != nil {
		return nil, err
	}
	return ContextMenuCategories(u)
}

func (r *k8sContextRepository) GetPackageRepository(ctx context.Context) (string, error) {
	u, err := r.catalogContext(ctx)
	if err != nil {
		return "", err
	}
	return ContextPackageRepository(u)
}

// ContextPlatformServices reads the services of a Context catalog, each with
// the title of the section it is nested in as category.
func ContextPlatformServices(u *unstructured.Unstructured) ([]models.PlatformService, error) {
	cats, found, err := catalogCategories(u)
	if err != nil {
		return nil, err
	}
	if !found {
		logrus.WithField("context", u.GetNamespace()+"/"+u.GetName()).Warn("No serviceCatalog.categories found in Context")
		return nil, nil
	}

	var services []models.PlatformService
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

// ContextMenuCategories reads the sections of a Context catalog, in order.
func ContextMenuCategories(u *unstructured.Unstructured) ([]models.MenuCategory, error) {
	cats, found, err := catalogCategories(u)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	var categories []models.MenuCategory
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

// ContextPackageRepository reads serviceCatalog.defaultRepository.
func ContextPackageRepository(u *unstructured.Unstructured) (string, error) {
	repo, _, _ := unstructured.NestedString(u.Object, "spec", "context", "serviceCatalog", "defaultRepository")
	if repo == "" {
		return "", fmt.Errorf("serviceCatalog.defaultRepository not found in Context %s/%s", u.GetNamespace(), u.GetName())
	}
	return repo, nil
}

func (r *k8sContextRepository) GetIngressSuffix(ctx context.Context) (string, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return "", err
	}
	suffix, _, _ := unstructured.NestedString(u.Object, "spec", "context", "ingress", "suffix")
	if suffix == "" {
		return "", fmt.Errorf("ingress.suffix not found in Context %s", r.platform)
	}
	return suffix, nil
}

// GetIdentity reads platform.identity. An absent block means the platform was
// never told, and the safe reading is that somebody else makes the client
// Secrets: provisioning them ourselves would post CRs on a cluster that never
// asked for them.
func (r *k8sContextRepository) GetIdentity(ctx context.Context) (*models.PlatformIdentity, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return nil, err
	}
	provisioning, _, _ := unstructured.NestedString(u.Object, "spec", "context", "oidc", "clientProvisioning")
	if provisioning == "" {
		// Legacy path, kept while Contexts migrate to the single oidc block.
		provisioning, _, _ = unstructured.NestedString(u.Object, "spec", "context", "identity", "clientProvisioning")
	}
	identity := &models.PlatformIdentity{
		ClientProvisioning: provisioning,
		KubauthNamespace:   kubauthNamespaceOf(u),
	}
	if identity.ClientProvisioning == "" {
		identity.ClientProvisioning = models.ClientProvisioningExisting
	}
	return identity, identity.Validate()
}

// GetKubauthNamespace answers where the kubauth CRs live, whichever key the
// Context declares it under. Whether clients are provisioned through kubauth is
// the provisioning provider's decision: gating the namespace on a second field
// is how the two ended up contradicting each other.
func (r *k8sContextRepository) GetKubauthNamespace(ctx context.Context) (string, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return "", err
	}
	namespace := kubauthNamespaceOf(u)
	if namespace == "" {
		return "", fmt.Errorf("identity.kubauth.namespace not found in Context %s", r.platform)
	}
	return namespace, nil
}

// kubauthNamespaceOf reads the kubauth namespace, most current key first.
func kubauthNamespaceOf(u *unstructured.Unstructured) string {
	for _, path := range [][]string{
		{"spec", "context", "identity", "kubauth", "namespace"},
		{"spec", "context", "oidc", "kubauth", "namespace"},
		{"spec", "context", "identity", "kubauthNamespace"},
	} {
		if namespace, _, _ := unstructured.NestedString(u.Object, path...); namespace != "" {
			return namespace
		}
	}
	return ""
}

func (r *k8sContextRepository) GetIdentityProvider(ctx context.Context) (string, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return "", err
	}
	provider, _, _ := unstructured.NestedString(u.Object, "spec", "context", "identity", "provider")
	return provider, nil
}

func (r *k8sContextRepository) GetIdentityOidcConfig(ctx context.Context) (*models.IdentityOidcConfig, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return nil, err
	}
	authority, _, _ := unstructured.NestedString(u.Object, "spec", "context", "identity", "oidc", "authority")
	clientID, _, _ := unstructured.NestedString(u.Object, "spec", "context", "identity", "oidc", "clientId")
	if authority == "" || clientID == "" {
		return nil, nil
	}
	scope, _, _ := unstructured.NestedString(u.Object, "spec", "context", "identity", "oidc", "scope")
	return &models.IdentityOidcConfig{
		Authority: authority,
		ClientID:  clientID,
		Scope:     scope,
	}, nil
}

// GetOidcIssuer reads the issuer from the Context, so a platform declaring its
// identity provider does not repeat it in the server's environment.
func (r *k8sContextRepository) GetOidcIssuer(ctx context.Context) (string, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return "", err
	}
	if authority, _, _ := unstructured.NestedString(u.Object, "spec", "context", "identity", "oidc", "authority"); authority != "" {
		return authority, nil
	}
	issuer, _, _ := unstructured.NestedString(u.Object, "spec", "context", "oidc", "issuerUri")
	return issuer, nil
}

// GetOidcClientID mirrors GetOidcIssuer's two layouts.
func (r *k8sContextRepository) GetOidcClientID(ctx context.Context) (string, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return "", err
	}
	if clientID, _, _ := unstructured.NestedString(u.Object, "spec", "context", "oidc", "clientId"); clientID != "" {
		return clientID, nil
	}
	clientID, _, _ := unstructured.NestedString(u.Object, "spec", "context", "identity", "oidc", "clientId")
	return clientID, nil
}

// GetOidcInsecureSkipVerify reads the platform's answer to a self-signed issuer
// certificate, the setting the Keycloak provisioner already honors.
func (r *k8sContextRepository) GetOidcInsecureSkipVerify(ctx context.Context) (bool, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return false, err
	}
	insecure, _, _ := unstructured.NestedBool(u.Object, "spec", "context", "oidc", "insecureSkipVerify")
	return insecure, nil
}

// GetIdentityProvisioningProvider names the backend that makes and unmakes the
// OAuth clients. A Context that only carries the older oidc.clientProvisioning
// block is honored: its kubauth mode means the same thing here, and anything
// else means the platform provisions no client of its own.
func (r *k8sContextRepository) GetIdentityProvisioningProvider(ctx context.Context) (string, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return "", err
	}
	provider, _, _ := unstructured.NestedString(u.Object, "spec", "context", "identity", "provisioning", "provider")
	if provider != "" {
		return provider, nil
	}

	identity, err := r.GetIdentity(ctx)
	if err != nil {
		return "", err
	}
	if identity.ProvisionsWithKubauth() {
		return provisioning.ProviderKubauth, nil
	}
	return provisioning.ProviderNone, nil
}

func (r *k8sContextRepository) GetKeycloakProvisioningConfig(ctx context.Context) (*provisioning.KeycloakConfig, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return nil, err
	}

	issuer, _, _ := unstructured.NestedString(u.Object, "spec", "context", "identity", "provisioning", "keycloak", "issuerUri")
	if issuer == "" {
		return nil, fmt.Errorf("identity.provisioning.keycloak.issuerUri not found in Context %s", r.platform)
	}

	secretPath := []string{"spec", "context", "identity", "provisioning", "keycloak", "credentialsSecret"}
	secretName, _, _ := unstructured.NestedString(u.Object, append(secretPath, "name")...)
	if secretName == "" {
		return nil, fmt.Errorf("identity.provisioning.keycloak.credentialsSecret.name not found in Context %s", r.platform)
	}
	secretNamespace, _, _ := unstructured.NestedString(u.Object, append(secretPath, "namespace")...)
	clientIDKey, _, _ := unstructured.NestedString(u.Object, append(secretPath, "clientIdKey")...)
	if clientIDKey == "" {
		clientIDKey = "client_id"
	}
	clientSecretKey, _, _ := unstructured.NestedString(u.Object, append(secretPath, "clientSecretKey")...)
	if clientSecretKey == "" {
		clientSecretKey = "client_secret"
	}
	insecure, _, _ := unstructured.NestedBool(u.Object, "spec", "context", "identity", "provisioning", "keycloak", "insecureSkipTLSVerify")

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

func (r *k8sContextRepository) GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return nil, err
	}

	result := make(map[string][]models.ProfileImage)
	profileTypes := []string{"jupyterlab", "vscode", "rstudio"}

	for _, pType := range profileTypes {
		rawImages, found, err := unstructured.NestedSlice(u.Object, "spec", "context", "jupyter", "profiles", pType, "images")
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

func (r *k8sContextRepository) GetSparkConfig(ctx context.Context) (*models.SparkConfig, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return nil, err
	}

	sparkOp, found, err := unstructured.NestedMap(u.Object, "spec", "context", "sparkOperator")
	if err != nil {
		return nil, fmt.Errorf("failed to read sparkOperator from Context: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("sparkOperator not found in Context %s", r.platform)
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

// getContext reads the platform Context.
func (r *k8sContextRepository) getContext(ctx context.Context) (*unstructured.Unstructured, error) {
	u, err := r.read(ctx, r.platform)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, apierrors.NewNotFound(contextGVR.GroupResource(), r.platform.Name)
	}
	return u, nil
}

// catalogContext reads the Context holding the catalog: the Control Plane
// one, or the platform one when the Control Plane Context does not exist or
// carries no serviceCatalog. Any other read error is returned, never
// answered from the platform Context.
func (r *k8sContextRepository) catalogContext(ctx context.Context) (*unstructured.Unstructured, error) {
	if r.controlPlane != r.platform {
		u, err := r.read(ctx, r.controlPlane)
		if err != nil {
			return nil, err
		}
		if u != nil && HasServiceCatalog(u) {
			return u, nil
		}
	}
	return r.getContext(ctx)
}

// read returns a Context, nil when it does not exist. Both answers are cached.
func (r *k8sContextRepository) read(ctx context.Context, ref ContextRef) (*unstructured.Unstructured, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if c, ok := r.cache[ref]; ok && time.Since(c.cachedAt) < contextCacheTTL {
		return c.object, nil
	}
	u, err := r.client.Resource(contextGVR).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		u, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.cache[ref] = cachedContext{object: u, cachedAt: time.Now()}
	return u, nil
}

// ReadServiceCatalog returns spec.context.serviceCatalog of the Control Plane
// Context, else of the platform one, false when neither carries one. A
// missing Context is not an error.
func ReadServiceCatalog(ctx context.Context, client dynamic.Interface, platform, controlPlane ContextRef) (map[string]interface{}, bool, error) {
	for _, ref := range []ContextRef{controlPlane, platform} {
		u, err := client.Resource(contextGVR).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("failed to read Context %s: %w", ref, err)
		}
		if catalog, found, _ := unstructured.NestedMap(u.Object, "spec", "context", "serviceCatalog"); found {
			return catalog, true, nil
		}
	}
	return nil, false, nil
}

// HasServiceCatalog reports whether a Context carries spec.context.serviceCatalog.
func HasServiceCatalog(u *unstructured.Unstructured) bool {
	_, found, err := unstructured.NestedMap(u.Object, "spec", "context", "serviceCatalog")
	return found && err == nil
}

func (r *k8sContextRepository) Invalidate() {
	r.mu.Lock()
	r.cache = map[ContextRef]cachedContext{}
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
