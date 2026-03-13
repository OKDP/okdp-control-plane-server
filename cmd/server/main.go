package main

import (
	"github.com/sirupsen/logrus"

	"github.com/okdp/okdp-server-new/internal/api/handlers"
	"github.com/okdp/okdp-server-new/internal/api/router"
	"github.com/okdp/okdp-server-new/internal/config"
	"github.com/okdp/okdp-server-new/internal/repository"
	"github.com/okdp/okdp-server-new/internal/service"
)

// @title           OKDP Server New API
// @version         1.0
// @description     Minimal API server for OKDP UI New
// @termsOfService  http://swagger.io/terms/

// @contact.name    API Support
// @contact.url     http://www.swagger.io/support
// @contact.email   support@swagger.io

// @license.name    Apache 2.0
// @license.url     http://www.apache.org/licenses/LICENSE-2.0.html

// @host            localhost:8093
// @BasePath        /
func main() {
	// Load Configuration
	cfg, err := config.Load()
	if err != nil {
		logrus.Fatalf("Failed to load configuration: %v", err)
	}

	// Initialize Logger
	level, err := logrus.ParseLevel(cfg.LogLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	logrus.SetLevel(level)
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	// Initialize Kubernetes Clients
	k8sClient, err := repository.InitK8sClient()
	if err != nil {
		logrus.Fatalf("Failed to initialize Kubernetes client: %v", err)
	}

	k8sTypedClient, err := repository.InitK8sTypedClient()
	if err != nil {
		logrus.Fatalf("Failed to initialize typed Kubernetes client: %v", err)
	}

	// Initialize Project stack (projects are Kubernetes Namespaces
	// carrying the label okdp.io/project)
	projectRepo := repository.NewProjectRepository(k8sTypedClient)
	contextWriterRepo := repository.NewContextWriterRepository(k8sClient, cfg.ContextName, cfg.ContextNamespace)
	projectService := service.NewDefaultProjectService(projectRepo, contextWriterRepo)
	projectHandler := handlers.NewProjectHandler(projectService)

	// Initialize Identity stack
	identityRepo := repository.NewIdentityRepository(k8sClient, cfg.PlatformNamespace)
	identityService := service.NewDefaultIdentityService(identityRepo)
	identityHandler := handlers.NewIdentityHandler(identityService)

	// Initialize SecretStore stack (namespace is dynamic per project)
	secretStoreRepo := repository.NewSecretStoreRepository(k8sClient)
	secretStoreService := service.NewDefaultSecretStoreService(secretStoreRepo)
	secretStoreHandler := handlers.NewSecretStoreHandler(secretStoreService)

	// Initialize ExternalSecret stack (namespace is dynamic per project)
	externalSecretRepo := repository.NewExternalSecretRepository(k8sClient)
	externalSecretService := service.NewDefaultExternalSecretService(externalSecretRepo, secretStoreRepo)
	externalSecretHandler := handlers.NewExternalSecretHandler(externalSecretService)

	// Initialize Service stack (KuboCD Releases + Context-driven catalog)
	serviceRepo := repository.NewServiceRepository(k8sClient)
	contextRepo := repository.NewContextRepository(k8sClient, cfg.ContextName, cfg.ContextNamespace)
	schemaService := service.NewDefaultPackageSchemaService(contextRepo)
	serviceService := service.NewDefaultServiceService(serviceRepo, contextRepo, contextWriterRepo, schemaService, k8sClient, k8sTypedClient, cfg.ContextNamespace, cfg.ReleaseInterval, cfg.ReleaseTimeout, cfg.ExcludedSidecarPrefixes)
	serviceHandler := handlers.NewServiceHandler(serviceService, schemaService)

	// Initialize Spark stack (SparkApplication CRUD)
	sparkRepo := repository.NewSparkAppRepository(k8sClient)
	sparkService := service.NewDefaultSparkService(sparkRepo, contextRepo, k8sTypedClient)
	sparkHandler := handlers.NewSparkHandler(sparkService)

	// Setup router
	r := router.SetupRouter(cfg, projectHandler, identityHandler, secretStoreHandler, externalSecretHandler, serviceHandler, sparkHandler)

	// Start Server
	logrus.WithField("port", cfg.ServerPort).Info("Starting server")
	if err := r.Run(":" + cfg.ServerPort); err != nil {
		logrus.Fatalf("Failed to start server: %v", err)
	}
}
