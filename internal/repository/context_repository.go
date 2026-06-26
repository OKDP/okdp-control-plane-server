package repository

import (
	"context"
	"fmt"

	"github.com/okdp/okdp-server-new/internal/models"
	"github.com/sirupsen/logrus"

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
	// GetPlatformServices returns the managed OKDP services (from spec.context.okdp.services).
	GetPlatformServices(ctx context.Context) ([]models.PlatformService, error)

	// GetCatalog returns the self-service catalog categories (from spec.context.okdp.catalogs).
	GetCatalog(ctx context.Context) ([]models.CatalogCategory, error)

	// GetPackageRepository returns the OCI package repository prefix (from spec.context.okdp.packageRepository).
	GetPackageRepository(ctx context.Context) (string, error)

	// GetIngressSuffix returns the ingress domain suffix (from spec.context.ingress.suffix).
	GetIngressSuffix(ctx context.Context) (string, error)

	// GetKubauthNamespace returns the namespace where kubauth resources live (from spec.context.kubauth.namespace).
	GetKubauthNamespace(ctx context.Context) (string, error)

	// GetProfileImages returns available images per profile type from spec.context.jupyter.profiles.
	GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error)

	// GetSparkConfig returns Spark operator configuration from spec.context.sparkOperator.
	GetSparkConfig(ctx context.Context) (*models.SparkConfig, error)
}

type k8sContextRepository struct {
	client    dynamic.Interface
	name      string
	namespace string
}

func NewContextRepository(client dynamic.Interface, name, namespace string) ContextRepository {
	return &k8sContextRepository{
		client:    client,
		name:      name,
		namespace: namespace,
	}
}

func (r *k8sContextRepository) GetPlatformServices(ctx context.Context) ([]models.PlatformService, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return nil, err
	}

	rawServices, found, err := unstructured.NestedSlice(u.Object, "spec", "context", "okdp", "services")
	if err != nil {
		return nil, fmt.Errorf("failed to read okdp.services from Context: %w", err)
	}
	if !found {
		logrus.Warn("No okdp.services found in Context CR")
		return nil, nil
	}

	var services []models.PlatformService
	for _, raw := range rawServices {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		defaultVersion := getString(m, "default")
		if defaultVersion == "" {
			defaultVersion = getString(m, "tag")
		}
		svc := models.PlatformService{
			Name:           getString(m, "name"),
			DefaultVersion: defaultVersion,
			Description:    getString(m, "description"),
			Icon:           getString(m, "icon"),
			Category:       getString(m, "category"),
			Repository:     getString(m, "repository"),
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
		services = append(services, svc)
	}
	return services, nil
}

func (r *k8sContextRepository) GetCatalog(ctx context.Context) ([]models.CatalogCategory, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return nil, err
	}

	rawCatalogs, found, err := unstructured.NestedSlice(u.Object, "spec", "context", "okdp", "catalogs")
	if err != nil {
		return nil, fmt.Errorf("failed to read okdp.catalogs from Context: %w", err)
	}
	if !found {
		return nil, nil
	}

	var categories []models.CatalogCategory
	for _, raw := range rawCatalogs {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}

		cat := models.CatalogCategory{
			ID:          getString(m, "id"),
			Name:        getString(m, "name"),
			Description: getString(m, "description"),
		}

		if pkgs, ok := m["packages"].([]interface{}); ok {
			for _, p := range pkgs {
				pm, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				cat.Packages = append(cat.Packages, models.CatalogPackage{
					Name: getString(pm, "name"),
					Tag:  getString(pm, "tag"),
				})
			}
		}

		categories = append(categories, cat)
	}
	return categories, nil
}

func (r *k8sContextRepository) GetPackageRepository(ctx context.Context) (string, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return "", err
	}

	repo, _, _ := unstructured.NestedString(u.Object, "spec", "context", "okdp", "packageRepository")
	if repo == "" {
		return "", fmt.Errorf("okdp.packageRepository not found in Context %s/%s", r.namespace, r.name)
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
		return "", fmt.Errorf("ingress.suffix not found in Context %s/%s", r.namespace, r.name)
	}
	return suffix, nil
}

func (r *k8sContextRepository) GetKubauthNamespace(ctx context.Context) (string, error) {
	u, err := r.getContext(ctx)
	if err != nil {
		return "", err
	}
	ns, _, _ := unstructured.NestedString(u.Object, "spec", "context", "kubauth", "namespace")
	if ns == "" {
		return "", fmt.Errorf("kubauth.namespace not found in Context %s/%s", r.namespace, r.name)
	}
	return ns, nil
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
		return nil, fmt.Errorf("sparkOperator not found in Context %s/%s", r.namespace, r.name)
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

func (r *k8sContextRepository) getContext(ctx context.Context) (*unstructured.Unstructured, error) {
	return r.client.Resource(contextGVR).Namespace(r.namespace).Get(ctx, r.name, metav1.GetOptions{})
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
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
