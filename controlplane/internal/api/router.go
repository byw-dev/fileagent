// Package api provides the Gin HTTP router and all route registrations for
// the Control Plane REST API. Phase 1 registers every route defined in
// system-design.md §5.11 with 501 Not Implemented handlers; actual
// implementations are added in Phase 2.
package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// RouterConfig carries the dependencies required by the router.
type RouterConfig struct {
	JWTSecret     string
	Logger        *zap.Logger
	JWTService    auth.Service   // nil → auth routes return 501 (Phase 1 behaviour)
	AuthDB        handler.AuthDB // nil → auth routes return 501
	UsersDB       handler.UsersDB
	FileTypesDB   handler.FileTypesDB
	FilesDB       handler.FilesDB
	MinIOSigner   handler.MinIOPresigner
	BucketsDB     handler.BucketsDB
	MinIOAdmin    handler.MinioBucketMaker // nil → bucket creation skips MinIO call
	EventRulesDB  handler.EventRulesDB
	UploadLogsDB  handler.UploadLogsDB
	AgentsDB      handler.AgentsDB
	AgentMgr      handler.AgentManager
	Dispatcher    handler.RuleDispatcher
	Registry      handler.AgentRegistryClient
	AgentCache    handler.AgentCacheClient // nil → is_online always false
	DirStore      handler.DirListingStore  // nil → list-dir returns 202 (legacy)
	DryRunStore   handler.DryRunStore      // nil → test-rule returns 501
	MinioIndexer  handler.IndexerClient    // nil → minio webhook events are only logged
	WebhookSecret string                   // shared secret for /internal/minio-event; empty → endpoint rejects all
	StatsDB       handler.StatsDB          // nil → stats endpoint returns 501

	// RateLimiter backs the per-user API rate-limit middleware. When nil, or
	// when RateLimitPerMinute <= 0, rate limiting is disabled.
	RateLimiter middleware.RateLimitStore
	// RateLimitPerMinute is the max authenticated requests per user per minute.
	RateLimitPerMinute int

	// WebUIFS, when non-nil, is the compiled Web UI (webui/dist) served for all
	// non-API routes with SPA fallback (see registerSPA). It is nil in the
	// default pure-API build and non-nil in the tag-`webui` bundled binary.
	WebUIFS fs.FS
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

	// ── Internal MinIO event webhook (authenticated by shared secret) ───────
	minioH := handler.NewMinioEventHandler(cfg.MinioIndexer, cfg.WebhookSecret, cfg.Logger)
	r.POST("/internal/minio-event", minioH.Handle)

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

	// Per-user API rate limiting runs after JWT so the caller's user ID is
	// known (system-design.md §5.1). Disabled when no limiter is wired or the
	// configured limit is non-positive.
	if cfg.RateLimiter != nil && cfg.RateLimitPerMinute > 0 {
		v1.Use(middleware.RateLimit(cfg.RateLimiter, cfg.RateLimitPerMinute, cfg.Logger))
	}

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
	if cfg.AgentCache != nil {
		agentsH.WithCache(cfg.AgentCache)
	}
	if cfg.DirStore != nil {
		agentsH.WithDirStore(cfg.DirStore)
	}
	if cfg.DryRunStore != nil {
		agentsH.WithDryRunStore(cfg.DryRunStore)
	}
	agents := v1.Group("/agents")
	{
		agents.GET("", agentsH.List)
		agents.GET("/:id", agentsH.Get)
		agents.PATCH("/:id", superAdmin, agentsH.Rename)
		agents.POST("/:id/approve", superAdmin, agentsH.Approve)
		agents.POST("/:id/revoke", superAdmin, agentsH.Revoke)
		agents.POST("/:id/list-dir", agentsH.ListDir)
		agents.POST("/:id/test-rule", agentsH.TestRule)
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
	bucketsH := handler.NewBucketsHandler(cfg.BucketsDB, cfg.MinIOAdmin, cfg.Logger)
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

	// Dashboard statistics
	statsH := handler.NewStatsHandler(cfg.StatsDB, cfg.Logger)
	v1.GET("/stats/dashboard", statsH.Dashboard)

	// Web UI (single-page app) served from the embedded filesystem, if present.
	// Registered last so it only handles routes not claimed by the API above.
	if cfg.WebUIFS != nil {
		registerSPA(r, cfg.WebUIFS)
	}

	return r
}

// registerSPA serves the embedded Web UI (a BrowserRouter single-page app) for
// every route the API did not claim. Static asset requests are served from
// fsys; unknown client-side routes fall back to index.html so deep links and
// hard refreshes work. Unmatched API-shaped paths still return a JSON 404 in
// the standard error envelope rather than the HTML shell, so a mistyped API
// call fails loudly instead of silently receiving index.html.
func registerSPA(r *gin.Engine, fsys fs.FS) {
	httpFS := http.FS(fsys)

	r.NoRoute(func(c *gin.Context) {
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found", nil)
			return
		}

		p := c.Request.URL.Path
		if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/internal/") || p == "/healthz" {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found", nil)
			return
		}

		// Serve a real static file when one exists (e.g. /assets/index-*.js);
		// otherwise fall back to the SPA shell for client-side routing.
		if name := strings.TrimPrefix(path.Clean(p), "/"); name != "" && fileExists(fsys, name) {
			c.FileFromFS(p, httpFS)
			return
		}
		serveIndex(c, fsys)
	})
}

// fileExists reports whether name resolves to a regular (non-directory) file in
// fsys. Directories return false so requests like "/" fall through to the SPA
// shell rather than yielding a directory listing.
func fileExists(fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// serveIndex writes index.html with a 200 status as the SPA fallback. A missing
// index.html (misconfigured embed) yields a 404 so the failure is visible.
func serveIndex(c *gin.Context, fsys fs.FS) {
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found", nil)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
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
