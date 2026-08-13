package main

import (
	"context"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/okdp/okdp-control-plane-server/internal/api/handlers"
	"github.com/okdp/okdp-control-plane-server/internal/api/router"
	"github.com/okdp/okdp-control-plane-server/internal/config"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
	"github.com/okdp/okdp-control-plane-server/internal/service"
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

	k8sDiscoveryClient, err := repository.InitK8sDiscoveryClient()
	if err != nil {
		logrus.Fatalf("Failed to initialize Kubernetes discovery client: %v", err)
	}

	// Initialize Project stack (projects are Kubernetes Namespaces
	// carrying the label okdp.io/project)
	projectRepo := repository.NewProjectRepository(k8sTypedClient)
	contextWriterRepo := repository.NewContextWriterRepository(k8sClient, cfg.ContextName, cfg.ContextNamespace)
	projectService := service.NewDefaultProjectService(projectRepo)
	projectHandler := handlers.NewProjectHandler(projectService)

	// Context repository (shared by capabilities, catalog and Spark)
	contextRepo := repository.NewContextRepository(k8sClient, cfg.ContextName, cfg.ContextNamespace)

	// Initialize Capabilities stack (platform features derived from the Context)
	capabilityService := service.NewDefaultCapabilityService(contextRepo)
	capabilitiesHandler := handlers.NewCapabilitiesHandler(capabilityService)

	// Initialize Identity stack. The namespace is read per call from the Context,
	// and the discovery client tells whether the kubauth CRDs are there at all.
	kubauthNamespace := func(ctx context.Context) string {
		if ns, err := contextRepo.GetKubauthNamespace(ctx); err == nil {
			return ns
		}
		return cfg.PlatformNamespace
	}
	identityRepo := repository.NewIdentityRepository(k8sClient, k8sDiscoveryClient, kubauthNamespace)
	identityService := service.NewDefaultIdentityService(identityRepo)
	identityHandler := handlers.NewIdentityHandler(identityService)

	// Initialize SecretStore stack (namespace is dynamic per project)
	secretStoreRepo := repository.NewSecretStoreRepository(k8sClient, k8sDiscoveryClient)
	secretStoreService := service.NewDefaultSecretStoreService(secretStoreRepo)
	secretStoreHandler := handlers.NewSecretStoreHandler(secretStoreService)

	// Initialize ExternalSecret stack (namespace is dynamic per project)
	externalSecretRepo := repository.NewExternalSecretRepository(k8sClient, k8sDiscoveryClient)
	externalSecretService := service.NewDefaultExternalSecretService(externalSecretRepo, secretStoreRepo)
	externalSecretHandler := handlers.NewExternalSecretHandler(externalSecretService)

	// Initialize Service stack (KuboCD Releases + Context-driven catalog)
	serviceRepo := repository.NewServiceRepository(k8sClient)
	schemaService := service.NewDefaultPackageSchemaService(contextRepo)
	schemaService.SetInsecureRegistries(cfg.InsecureOCIRegistries)
	// OIDC client provisioning (backend selected per call from the Context)
	oidcProvisioner := provisioning.NewContextSelector(contextRepo, k8sClient)
	serviceService := service.NewDefaultServiceService(serviceRepo, contextRepo, contextWriterRepo, schemaService, oidcProvisioner, k8sClient, k8sTypedClient, cfg.ContextNamespace, cfg.ReleaseInterval, cfg.ReleaseTimeout, cfg.ExcludedSidecarPrefixes)
	serviceService.SetInsecureRegistries(cfg.InsecureOCIRegistries)
	serviceHandler := handlers.NewServiceHandler(serviceService, schemaService)

	// Initialize Spark stack (SparkApplication CRUD)
	sparkRepo := repository.NewSparkAppRepository(k8sClient)
	sparkService := service.NewDefaultSparkService(sparkRepo, contextRepo, k8sTypedClient)
	sparkHandler := handlers.NewSparkHandler(sparkService)

	// Setup router
	// Initialize Connection stack (external connections declared by users +
	// internal ones derived from the services deployed in a project)
	contractCatalog, err := service.NewEmbeddedContractCatalog()
	if err != nil {
		logrus.Fatalf("Failed to load the contract catalog: %v", err)
	}
	connectionRepo := repository.NewConnectionRepository(k8sClient, k8sTypedClient, k8sDiscoveryClient)
	connectionService := service.NewDefaultConnectionService(connectionRepo, serviceRepo, contractCatalog)
	connectionHandler := handlers.NewConnectionHandler(connectionService)

	// The identity block is checked once, at startup, against what the cluster
	// actually serves. A platform told to provision clients through kubauth on a
	// cluster without kubauth would deploy services whose OIDC client is never
	// created, and fail much later at the point of authenticating a user, with
	// nothing pointing back here.
	checkIdentityConfiguration(context.Background(), contextRepo, identityRepo)

	r := router.SetupRouter(cfg, capabilitiesHandler, projectHandler, identityHandler, secretStoreHandler, externalSecretHandler, serviceHandler, sparkHandler, connectionHandler)

	// Start Server
	//
	// Not r.Run: it leaves every timeout at zero, so a client that opens a
	// connection and sends its headers one byte at a time holds a goroutine
	// indefinitely, and an idle keep-alive is never reclaimed.
	//
	// WriteTimeout stays unset on purpose. Three routes stream Server-Sent
	// Events (projects, services, spark apps), and a write deadline would cut
	// them mid-flight, which is exactly what they are for.
	server := &http.Server{
		Addr:              ":" + cfg.ServerPort,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	logrus.WithField("port", cfg.ServerPort).Info("Starting server")
	if err := server.ListenAndServe(); err != nil {
		logrus.Fatalf("Failed to start server: %v", err)
	}
}

// checkIdentityConfiguration fails fast on an identity block that cannot hold,
// and stays quiet otherwise. A Context that cannot be read at all is not fatal:
// the platform Context may simply not be there yet on a fresh cluster, and the
// routes that need it already report their own absence.
func checkIdentityConfiguration(ctx context.Context, contextRepo repository.ContextRepository, identityRepo repository.IdentityRepository) {
	identity, err := contextRepo.GetIdentity(ctx)
	if err != nil {
		// A block that is present and wrong is a configuration error worth
		// stopping for; the two are told apart by the error the repository
		// returns, which only validates what it found.
		if identity != nil {
			logrus.Fatalf("Invalid oidc client-provisioning configuration: %v", err)
		}
		logrus.WithError(err).Warn("Could not read oidc.clientProvisioning, assuming clients are provisioned elsewhere")
		return
	}

	if identity.ProvisionsWithKubauth() && !identityRepo.Available(ctx) {
		logrus.Fatalf(
			"oidc.clientProvisioning is %q but the kubauth CRDs are not installed on this cluster. "+
				"Install kubauth, or set it to %q if another mechanism makes the OAuth client Secrets.",
			models.ClientProvisioningKubauth, models.ClientProvisioningExisting)
	}

	logrus.WithField("clientProvisioning", identity.ClientProvisioning).Info("Identity configuration accepted")
}
