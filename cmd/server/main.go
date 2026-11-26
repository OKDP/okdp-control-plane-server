package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/okdp/okdp-control-plane-server/internal/api/handlers"
	"github.com/okdp/okdp-control-plane-server/internal/api/router"
	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/config"
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

// @securityDefinitions.apikey BearerAuth
// @in                         header
// @name                       Authorization
// @description                OIDC access token, as "Bearer <token>". Required on
// @description                every /api route except /api/capabilities.

// @security                   BearerAuth
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

	engineCfg, err := loadEngineConfig(cfg.EngineType, os.LookupEnv)
	if err != nil {
		logrus.Fatalf("Invalid deployment engine configuration: %v", err)
	}
	logrus.WithField("engine", cfg.EngineType).Info("Deployment engine configuration accepted")

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

	appCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	platformRef := repository.ContextRef{Name: cfg.PlatformContextName, Namespace: cfg.PlatformContextNamespace}
	controlPlaneRef := repository.ContextRef{Name: cfg.ControlPlaneContextName, Namespace: cfg.ControlPlaneContextNamespace}

	// The stores of the desired state: the cluster, or git with a sync tool
	// applying the branch.
	state, err := buildDesiredState(appCtx, clients{dynamic: k8sClient, typed: k8sTypedClient, discovery: k8sDiscoveryClient}, stateOptions{
		engine:             engineCfg.kubocd,
		platform:           platformRef,
		controlPlane:       controlPlaneRef,
		insecureRegistries: cfg.InsecureOCIRegistries,
		registryAuthFile:   cfg.RegistryAuthFile,
	})
	if err != nil {
		logrus.Fatalf("Failed to initialize the desired state: %v", err)
	}
	logrus.WithField("persistence", engineCfg.kubocd.Persistence.Mode).Info("Desired state stores ready")
	state.start(appCtx)

	// Initialize Project stack (projects are Kubernetes Namespaces carrying
	// the label okdp.io/project, committed to git in git mode)
	projectService := service.NewDefaultProjectService(state.projects)
	projectHandler := handlers.NewProjectHandler(projectService)

	// Context repository (shared by capabilities, catalog and Spark)
	contextRepo := state.context

	// Initialize Identity stack. The namespace is read per call from the Context,
	// and the discovery client tells whether the kubauth CRDs are there at all.
	kubauthNamespace := func(ctx context.Context) string {
		ns, err := contextRepo.GetKubauthNamespace(ctx)
		if err == nil {
			return ns
		}
		// Falling back silently would read and write the CRs in a namespace the
		// kubauth controller does not watch, and answer 200 all along.
		logrus.WithError(err).Warnf("Could not read the kubauth namespace, falling back to %s", cfg.PlatformNamespace)
		return cfg.PlatformNamespace
	}
	identityRepo := repository.NewIdentityRepository(k8sClient, k8sDiscoveryClient, kubauthNamespace)
	identityService := service.NewDefaultIdentityService(identityRepo)
	identityHandler := handlers.NewIdentityHandler(identityService)

	// Initialize SecretStore stack (namespace is dynamic per project)
	secretStoreService := service.NewDefaultSecretStoreService(state.secretStores)
	secretStoreHandler := handlers.NewSecretStoreHandler(secretStoreService)

	// Initialize ExternalSecret stack (namespace is dynamic per project)
	externalSecretService := service.NewDefaultExternalSecretService(state.externalSecrets, state.secretStores)
	externalSecretHandler := handlers.NewExternalSecretHandler(externalSecretService)

	// Deployment engine: KuboCD Releases, catalog and platform settings
	serviceRepo := repository.NewServiceRepository(k8sClient)
	deploymentEngine := state.engine

	// Initialize Capabilities stack (platform features derived from the Context,
	// and the deployment engine). Built after the identity repository: the
	// identity capability answers on the same two conditions as the routes, the
	// configured provider and the CRDs actually being served.
	capabilityService := service.NewDefaultCapabilityService(contextRepo, identityRepo.Available, deploymentEngine.Info)
	capabilitiesHandler := handlers.NewCapabilitiesHandler(capabilityService)

	// Initialize Service stack (instances through the engine, pods and metrics from the cluster)
	// OIDC client provisioning (backend selected per call from the Context)
	oidcProvisioner := provisioning.NewContextSelector(contextRepo, k8sClient)
	serviceService := service.NewDefaultServiceService(deploymentEngine, oidcProvisioner, k8sClient, k8sTypedClient, cfg.ExcludedSidecarPrefixes)
	serviceHandler := handlers.NewServiceHandler(serviceService, deploymentEngine.Catalog)

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
	connectionService := service.NewDefaultConnectionService(state.connections, serviceRepo, contractCatalog)
	connectionHandler := handlers.NewConnectionHandler(connectionService)

	// The identity block is checked once, at startup, against what the cluster
	// actually serves. A platform told to provision clients through kubauth on a
	// cluster without kubauth would deploy services whose OIDC client is never
	// created, and fail much later at the point of authenticating a user, with
	// nothing pointing back here.
	checkIdentityConfiguration(appCtx, contextRepo, identityRepo)

	verifier := buildTokenVerifier(appCtx, cfg, contextRepo)

	r := router.SetupRouter(cfg, verifier, appCtx, capabilitiesHandler, projectHandler, identityHandler, secretStoreHandler, externalSecretHandler, serviceHandler, sparkHandler, connectionHandler)

	// Start Server
	//
	// Not r.Run: it leaves every timeout at zero, so a client that opens a
	// connection and sends its headers one byte at a time holds a goroutine
	// indefinitely, and an idle keep-alive is never reclaimed.
	//
	// WriteTimeout stays unset on purpose. Three routes stream Server-Sent
	// Events (projects, services, spark apps), and a write deadline would cut
	// them mid-flight, which is exactly what they are for. Only those streaming
	// routes derive from appCtx (via router middleware), so a stop signal ends
	// SSE streams immediately while in-flight REST writes (such as git pushes)
	// keep their request context and drain cleanly during server.Shutdown.
	server := &http.Server{
		Addr:              ":" + cfg.ServerPort,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		logrus.Fatalf("Failed to listen on %s: %v", server.Addr, err)
	}
	logrus.WithField("port", cfg.ServerPort).Info("Starting server")
	serveErr := serve(appCtx, server, listener, shutdownTimeout)
	stop()
	state.wait()
	if serveErr != nil {
		logrus.Fatalf("Server stopped: %v", serveErr)
	}
	logrus.Info("Server stopped")
}

const shutdownTimeout = 35 * time.Second

// serve runs server on listener until ctx ends, then shuts it down, waiting
// timeout at most for the requests in flight before closing them.
func serve(ctx context.Context, server *http.Server, listener net.Listener, timeout time.Duration) error {
	failed := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
		close(failed)
	}()
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		logrus.WithError(err).Warn("Requests still running at shutdown, closing them")
		_ = server.Close()
	}
	return <-failed
}

// checkIdentityConfiguration fails fast on an identity block that cannot hold,
// and stays quiet otherwise. A Context that cannot be read at all is not fatal:
// the platform Context may simply not be there yet on a fresh cluster, and the
// routes that need it already report their own absence.
func checkIdentityConfiguration(ctx context.Context, contextRepo repository.ContextRepository, identityRepo repository.IdentityRepository) {
	identity, err := contextRepo.GetIdentity(ctx)
	if err != nil {
		// A block that is present and wrong is a configuration error worth
		// stopping for. The two are told apart by the error the repository
		// returns, which only validates what it found.
		if identity != nil {
			logrus.Fatalf("Invalid oidc client-provisioning configuration: %v", err)
		}
		logrus.WithError(err).Warn("Could not read oidc.clientProvisioning, assuming clients are provisioned elsewhere")
		return
	}

	// The check follows the field the provisioner actually resolves from, so a
	// platform accepted here is a platform whose clients really get unmade.
	provider, err := contextRepo.GetIdentityProvisioningProvider(ctx)
	if err != nil {
		logrus.WithError(err).Warn("Could not read identity.provisioning.provider, assuming clients are provisioned elsewhere")
		return
	}

	// Every provider is checked here, not just kubauth: an unsupported value or
	// an incomplete Keycloak block would otherwise start fine and fail at the
	// first client cleanup, leaving stale OIDC clients behind.
	switch provider {
	case "", provisioning.ProviderNone:
	case provisioning.ProviderKubauth:
		if !identityRepo.Available(ctx) {
			logrus.Fatalf(
				"identity.provisioning.provider is %q but the kubauth CRDs are not installed on this cluster. "+
					"Install kubauth, or set it to %q if another mechanism makes the OAuth client Secrets.",
				provisioning.ProviderKubauth, provisioning.ProviderNone)
		}
		if _, err := contextRepo.GetKubauthNamespace(ctx); err != nil {
			logrus.Fatalf("identity.provisioning.provider is %q but the kubauth namespace is unreadable: %v",
				provisioning.ProviderKubauth, err)
		}
	case provisioning.ProviderKeycloak:
		if _, err := contextRepo.GetKeycloakProvisioningConfig(ctx); err != nil {
			logrus.Fatalf("identity.provisioning.provider is %q but its configuration is unusable: %v",
				provisioning.ProviderKeycloak, err)
		}
	default:
		logrus.Fatalf("identity.provisioning.provider is %q, which is not supported. Use %q, %q or %q.",
			provider, provisioning.ProviderNone, provisioning.ProviderKubauth, provisioning.ProviderKeycloak)
	}

	logrus.WithField("provisioningProvider", provider).Info("Identity configuration accepted")
}

// buildTokenVerifier resolves the issuer the API will trust, or returns nil
// when the deployment asked to run without verification.
func buildTokenVerifier(ctx context.Context, cfg *config.Config, contextRepo repository.ContextRepository) auth.Verifier {
	if cfg.OIDC.Disabled {
		logrus.Warn("AUTH_DISABLED is set: the API accepts unauthenticated requests. Never do this outside local development.")
		return nil
	}

	issuer, err := auth.ResolveIssuer(ctx, cfg.OIDC.Issuer, contextRepo.GetOidcIssuer)
	if err != nil {
		logrus.Fatalf("Failed to resolve the OIDC issuer: %v", err)
	}

	clientID, err := auth.ResolveClientID(ctx, cfg.OIDC.ClientID, contextRepo.GetOidcClientID)
	if err != nil {
		logrus.Fatalf("Failed to resolve the OIDC client id: %v", err)
	}

	insecure, err := contextRepo.GetOidcInsecureSkipVerify(ctx)
	if err != nil {
		logrus.WithError(err).Warn("Could not read oidc.insecureSkipVerify, verifying the issuer certificate")
	}
	if insecure {
		logrus.Warn("oidc.insecureSkipVerify is set: the issuer's certificate is not checked. Trust its CA in the pod instead, anywhere that is not a sandbox.")
	}

	verifier, err := auth.NewVerifier(ctx, auth.Config{
		Issuer:             issuer,
		InsecureSkipVerify: insecure,
		ClientID:           clientID,
	})
	if err != nil {
		logrus.Fatalf("Failed to initialize token verification: %v", err)
	}

	logrus.WithField("issuer", issuer).Info("API token verification enabled")
	return verifier
}
