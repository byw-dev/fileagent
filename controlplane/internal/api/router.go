// Package api provides the Gin HTTP router and all route registrations for
// the Control Plane REST API. Phase 1 registers every route defined in
// system-design.md §5.11 with 501 Not Implemented handlers; actual
// implementations are added in Phase 2.
package api

import (
	"net/http"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// RouterConfig carries the dependencies required by the router.
type RouterConfig struct {
	JWTSecret  string
	Logger     *zap.Logger
	JWTService auth.Service   // nil → auth routes return 501 (Phase 1 behaviour)
	AuthDB     handler.AuthDB // nil → auth routes return 501
	UsersDB     handler.UsersDB
	FileTypesDB handler.FileTypesDB
	FilesDB       handler.FilesDB
	MinIOSigner   handler.MinIOPresigner
	BucketsDB     handler.BucketsDB
	EventRulesDB  handler.EventRulesDB
	UploadLogsDB  handler.UploadLogsDB
	AgentsDB      handler.AgentsDB
	AgentMgr      handler.AgentManager
	Dispatcher    handler.RuleDispatcher
	Registry      handler.AgentRegistryClient
}

// NewRouter creates and fully configures a *gin.Engine with all routes and
// middleware registered. The engine can be served directly or used in tests.
func NewRouter(cfg RouterConfig) *gin.Engine {
	r := gin.New()

	// ── Global middleware ────────────────────────────────────────────────────
	r.Use(gin.Recovery())
	r.Use(middleware.RequestID())
	r.Use(ginZapLogger(cfg.Logger))

	// ── Health check (no auth required) ─────────────────────────────────────
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// ── Internal MinIO event webhook (no auth, secured by shared secret) ────
	r.POST("/internal/minio-event", middleware.NotImplemented)

	// ── Auth routes ──────────────────────────────────────────────────────────
	authH := handler.NewAuthHandler(cfg.JWTService, cfg.AuthDB)
	auth := r.Group("/api/auth")
	{
		auth.POST("/login", authH.Login)
		auth.POST("/refresh", authH.Refresh)
		auth.POST("/logout", authH.Logout)
		auth.GET("/me", authH.Me)
		auth.GET("/oidc/callback", authH.OIDCCallback)
	}

	// ── Authenticated API v1 routes ──────────────────────────────────────────
	jwtMW := middleware.JWT(cfg.JWTService, cfg.Logger)

	v1 := r.Group("/api/v1", jwtMW)

	// Users (super_admin only)
	usersH := handler.NewUsersHandler(cfg.UsersDB, cfg.Logger)
	superAdmin := middleware.RequireRole("super_admin")
	users := v1.Group("/users")
	{
		users.GET("", superAdmin, usersH.List)
		users.POST("", superAdmin, usersH.Create)
		users.PUT("/:id", superAdmin, usersH.Update)
		users.DELETE("/:id", superAdmin, usersH.Delete)
		users.PUT("/:id/password", usersH.UpdatePassword)
	}

	// Agents
	agentsH := handler.NewAgentsHandler(cfg.AgentsDB, cfg.AgentMgr, cfg.Dispatcher, cfg.Registry, cfg.Logger)
	agents := v1.Group("/agents")
	{
		agents.GET("", agentsH.List)
		agents.GET("/:id", agentsH.Get)
		agents.POST("/:id/approve", superAdmin, agentsH.Approve)
		agents.POST("/:id/revoke", superAdmin, agentsH.Revoke)
		agents.POST("/:id/list-dir", agentsH.ListDir)
		agents.GET("/:id/rules", agentsH.ListRules)
		agents.POST("/:id/rules", agentsH.CreateRule)
		agents.PUT("/:id/rules/:rid", agentsH.UpdateRule)
		agents.DELETE("/:id/rules/:rid", agentsH.DeleteRule)
		agents.GET("/:id/upload-logs", agentsH.ListUploadLogs)
	}

	// Files and file types
	filesH := handler.NewFilesHandler(cfg.FilesDB, cfg.MinIOSigner, cfg.Logger)
	fileTypesH := handler.NewFileTypesHandler(cfg.FileTypesDB, cfg.Logger)

	files := v1.Group("/files")
	{
		files.GET("", filesH.List)
		files.GET("/:id", filesH.Get)
		files.GET("/:id/download-url", filesH.DownloadURL)
		files.POST("/batch-download-urls", filesH.BatchDownloadURLs)
	}

	fileTypes := v1.Group("/file-types")
	{
		fileTypes.GET("", fileTypesH.List)
		fileTypes.POST("", fileTypesH.Create)
		fileTypes.PUT("/:id", fileTypesH.Update)
		fileTypes.DELETE("/:id", fileTypesH.Delete)
	}

	// Buckets
	bucketsH := handler.NewBucketsHandler(cfg.BucketsDB, cfg.Logger)
	buckets := v1.Group("/buckets")
	{
		buckets.GET("", bucketsH.List)
		buckets.POST("", superAdmin, bucketsH.Create)
	}

	// Event rules
	eventRulesH := handler.NewEventRulesHandler(cfg.EventRulesDB, cfg.Logger)
	eventRules := v1.Group("/event-rules")
	{
		eventRules.GET("", eventRulesH.List)
		eventRules.POST("", eventRulesH.Create)
		eventRules.PUT("/:id", eventRulesH.Update)
		eventRules.DELETE("/:id", eventRulesH.Delete)
		eventRules.GET("/:id/deliveries", eventRulesH.ListDeliveries)
	}

	// Upload logs
	uploadLogsH := handler.NewUploadLogsHandler(cfg.UploadLogsDB, cfg.Logger)
	uploadLogs := v1.Group("/upload-logs")
	{
		uploadLogs.GET("", uploadLogsH.List)
		uploadLogs.GET("/:id", uploadLogsH.Get)
	}

	return r
}

// ginZapLogger returns a Gin middleware that logs each request using zap.
func ginZapLogger(logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		logger.Info("http request",
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", c.Writer.Status()),
			zap.String("request_id", c.GetString("request_id")),
		)
	}
}
