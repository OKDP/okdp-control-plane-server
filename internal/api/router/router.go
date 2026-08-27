package router

import (
	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	"github.com/okdp/okdp-control-plane-server/internal/api/handlers"
	"github.com/okdp/okdp-control-plane-server/internal/api/middleware"
	"github.com/okdp/okdp-control-plane-server/internal/config"
)

// SetupRouter initializes the Gin router and defines routes
func SetupRouter(cfg *config.Config, capabilitiesHandler *handlers.CapabilitiesHandler, projectHandler *handlers.ProjectHandler, identityHandler *handlers.IdentityHandler, secretStoreHandler *handlers.SecretStoreHandler, externalSecretHandler *handlers.ExternalSecretHandler, serviceHandler *handlers.ServiceHandler, sparkHandler *handlers.SparkHandler, connectionHandler *handlers.ConnectionHandler) *gin.Engine {
	r := gin.New() // Use New() to skip default logger/recovery (we add them manually)

	// Middleware
	r.Use(middleware.RequestLogger())
	r.Use(gin.Recovery())
	r.Use(corsMiddleware(cfg))

	// Swagger documentation
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// Health check
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// API Routes
	api := r.Group("/api")
	{
		// Platform capabilities (UI feature discovery)
		api.GET("/capabilities", capabilitiesHandler.GetCapabilities)

		// Projects (backed by Kubernetes Namespaces)
		api.GET("/projects", projectHandler.ListProjects)
		api.GET("/projects/stream", projectHandler.StreamProjects)
		api.POST("/projects", projectHandler.CreateProject)
		api.GET("/projects/:name", projectHandler.GetProject)
		api.PUT("/projects/:name", projectHandler.UpdateProject)
		api.DELETE("/projects/:name", projectHandler.DeleteProject)

		// Identity. Two independent conditions, both required: the configured
		// provider must be kubauth, and its CRDs must actually be installed. A
		// cluster can declare the provider without carrying them. Guarded once
		// here rather than in each handler, so a route added later cannot slip
		// through unguarded.
		identity := api.Group("/v1/identity",
			capabilitiesHandler.RequireIdentityAPI(
				func(c *gin.Context) bool { return identityHandler.Available(c.Request.Context()) },
			),
		)
		{
			// Users
			identity.GET("/users", identityHandler.ListUsers)
			identity.GET("/users/:name", identityHandler.GetUser)
			identity.POST("/users", identityHandler.CreateUser)
			identity.PUT("/users/:name", identityHandler.UpdateUser)
			identity.DELETE("/users/:name", identityHandler.DeleteUser)

			// Groups
			identity.GET("/groups", identityHandler.ListGroups)
			identity.POST("/groups", identityHandler.CreateGroup)
			identity.PUT("/groups/:name", identityHandler.UpdateGroup)
			identity.DELETE("/groups/:name", identityHandler.DeleteGroup)
		}

		// Secret Stores (scoped per project namespace)
		requireProject := middleware.RequireProject(projectHandler.Resolve)

		// Both groups rest on the external-secrets CRDs, absent from a cluster
		// with no vault integration. Each group is gated on the CRD it actually
		// uses: a rolling install can serve one and not the other, and the
		// answer must be the 501 rather than a failure deeper in.
		esoUnavailable := "Vault integration is not available on this cluster: the external-secrets CRDs are not installed."
		requireSecretStores := handlers.RequireFeature(
			func(c *gin.Context) bool { return secretStoreHandler.Available(c.Request.Context()) },
			"external-secrets",
			esoUnavailable,
		)
		requireExternalSecrets := handlers.RequireFeature(
			func(c *gin.Context) bool { return externalSecretHandler.Available(c.Request.Context()) },
			"external-secrets",
			esoUnavailable,
		)

		secretStores := api.Group("/projects/:name/secret-stores", requireProject, requireSecretStores)
		{
			secretStores.GET("", secretStoreHandler.ListSecretStores)
			secretStores.POST("", secretStoreHandler.CreateSecretStore)
			secretStores.POST("/test", secretStoreHandler.TestConnection)
			secretStores.PUT("/:storeName", secretStoreHandler.UpdateSecretStore)
			secretStores.DELETE("/:storeName", secretStoreHandler.DeleteSecretStore)
			secretStores.GET("/:storeName/status", secretStoreHandler.GetStatus)
		}

		// External Secrets (scoped per project namespace)
		externalSecrets := api.Group("/projects/:name/external-secrets", requireProject, requireExternalSecrets)
		{
			externalSecrets.GET("", externalSecretHandler.ListExternalSecrets)
			externalSecrets.POST("", externalSecretHandler.CreateExternalSecret)
			externalSecrets.PUT("/:esName", externalSecretHandler.UpdateExternalSecret)
			externalSecrets.DELETE("/:esName", externalSecretHandler.DeleteExternalSecret)
			externalSecrets.GET("/:esName/status", externalSecretHandler.GetExternalSecretStatus)
			externalSecrets.POST("/check", externalSecretHandler.CheckRemoteRef)
		}

		// Contracts available for creation, and whether the KuboCD connection
		// CRDs are installed (external connections need them).
		api.GET("/contracts", connectionHandler.GetContracts)

		// Connections (scoped per project namespace). "internal" lists what the
		// project's deployed services expose. The rest are user-declared.
		connections := api.Group("/projects/:name/connections", requireProject)
		{
			connections.GET("", connectionHandler.ListConnections)
			connections.GET("/internal", connectionHandler.ListInternalConnections)
			connections.GET("/selectable", connectionHandler.ListSelectable)
			connections.GET("/:connName/consumers", connectionHandler.ListConsumers)
			connections.POST("", connectionHandler.CreateConnection)
			connections.POST("/test", connectionHandler.TestConnection)
			connections.PUT("/:connName", connectionHandler.UpdateConnection)
			connections.DELETE("/:connName", connectionHandler.DeleteConnection)
		}

		// Platform services (managed OKDP services)
		api.GET("/platform-services", serviceHandler.GetPlatformServices)
		api.GET("/platform-categories", serviceHandler.GetMenuCategories)
		api.POST("/platform-services", serviceHandler.CreatePlatformService)
		api.PUT("/platform-services/:serviceName", serviceHandler.UpdatePlatformService)
		api.DELETE("/platform-services/:serviceName", serviceHandler.DeletePlatformService)
		api.GET("/platform-services/:serviceName/versions", serviceHandler.GetServiceVersions)
		api.GET("/platform-services/:serviceName/schema", serviceHandler.GetServiceSchema)
		api.GET("/platform-services/:serviceName/inputs", serviceHandler.GetServiceInputs)
		api.GET("/profile-images", serviceHandler.GetProfileImages)

		// Deployed services per project (KuboCD Releases)
		services := api.Group("/projects/:name/services", requireProject)
		{
			services.GET("", serviceHandler.ListServices)
			services.GET("/stream", serviceHandler.StreamServices)
			services.POST("", serviceHandler.DeployService)
			services.GET("/:serviceName", serviceHandler.GetService)
			services.PATCH("/:serviceName/parameters", serviceHandler.UpdateServiceParameters)
			services.DELETE("/:serviceName", serviceHandler.DeleteService)
			services.GET("/:serviceName/pods", serviceHandler.ListPods)
			services.GET("/:serviceName/pods/:podName/logs", serviceHandler.GetPodLogs)
			services.GET("/:serviceName/metrics", serviceHandler.GetServiceMetrics)
		}
		api.GET("/projects/:name/metrics", requireProject, serviceHandler.GetProjectMetrics)

		// Spark config (from Context) + CRD schema
		api.GET("/spark-config", sparkHandler.GetSparkConfig)
		api.GET("/spark-app-schema", sparkHandler.GetSparkAppSchema)

		// SparkApplications per project
		sparkApps := api.Group("/projects/:name/spark-apps", requireProject)
		{
			sparkApps.GET("", sparkHandler.ListSparkApps)
			sparkApps.GET("/stream", sparkHandler.StreamSparkApps)
			sparkApps.POST("", sparkHandler.SubmitSparkApp)
			sparkApps.POST("/yaml", sparkHandler.SubmitSparkAppYAML)
			sparkApps.GET("/:appName", sparkHandler.GetSparkApp)
			sparkApps.PUT("/:appName", sparkHandler.UpdateSparkApp)
			sparkApps.DELETE("/:appName", sparkHandler.DeleteSparkApp)
			sparkApps.GET("/:appName/logs", sparkHandler.GetSparkAppLogs)
			sparkApps.GET("/:appName/ui", sparkHandler.GetSparkUI)
		}
	}

	return r
}

func corsMiddleware(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", cfg.AllowedOrigins)
		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, DELETE, PATCH")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}
