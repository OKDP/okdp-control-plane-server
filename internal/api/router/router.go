package router

import (
	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	"github.com/okdp/okdp-server-new/internal/api/handlers"
	"github.com/okdp/okdp-server-new/internal/api/middleware"
	"github.com/okdp/okdp-server-new/internal/config"
)

// SetupRouter initializes the Gin router and defines routes
func SetupRouter(cfg *config.Config, projectHandler *handlers.ProjectHandler, identityHandler *handlers.IdentityHandler, secretStoreHandler *handlers.SecretStoreHandler, externalSecretHandler *handlers.ExternalSecretHandler, serviceHandler *handlers.ServiceHandler, sparkHandler *handlers.SparkHandler) *gin.Engine {
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
		// Projects (backed by Kubernetes Namespaces)
		api.GET("/projects", projectHandler.ListProjects)
		api.GET("/projects/stream", projectHandler.StreamProjects)
		api.POST("/projects", projectHandler.CreateProject)
		api.GET("/projects/:name", projectHandler.GetProject)
		api.DELETE("/projects/:name", projectHandler.DeleteProject)

		// Identity
		identity := api.Group("/v1/identity")
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
		secretStores := api.Group("/projects/:name/secret-stores")
		{
			secretStores.GET("", secretStoreHandler.ListSecretStores)
			secretStores.POST("", secretStoreHandler.CreateSecretStore)
			secretStores.POST("/test", secretStoreHandler.TestConnection)
			secretStores.PUT("/:storeName", secretStoreHandler.UpdateSecretStore)
			secretStores.DELETE("/:storeName", secretStoreHandler.DeleteSecretStore)
			secretStores.GET("/:storeName/status", secretStoreHandler.GetStatus)
		}

		// External Secrets (scoped per project namespace)
		externalSecrets := api.Group("/projects/:name/external-secrets")
		{
			externalSecrets.GET("", externalSecretHandler.ListExternalSecrets)
			externalSecrets.POST("", externalSecretHandler.CreateExternalSecret)
			externalSecrets.PUT("/:esName", externalSecretHandler.UpdateExternalSecret)
			externalSecrets.DELETE("/:esName", externalSecretHandler.DeleteExternalSecret)
			externalSecrets.GET("/:esName/status", externalSecretHandler.GetExternalSecretStatus)
		}

		// Platform services (managed OKDP services)
		api.GET("/platform-services", serviceHandler.GetPlatformServices)
		api.GET("/platform-services/:serviceName/versions", serviceHandler.GetServiceVersions)
		api.GET("/platform-services/:serviceName/schema", serviceHandler.GetServiceSchema)
		api.GET("/profile-images", serviceHandler.GetProfileImages)

		// Deployed services per project (KuboCD Releases)
		services := api.Group("/projects/:name/services")
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

		// Self-service catalog (additional packages, no OKDP management)
		api.GET("/catalog", serviceHandler.GetCatalog)

		// Spark config (from Context) + CRD schema
		api.GET("/spark-config", sparkHandler.GetSparkConfig)
		api.GET("/spark-app-schema", sparkHandler.GetSparkAppSchema)

		// SparkApplications per project
		sparkApps := api.Group("/projects/:name/spark-apps")
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
