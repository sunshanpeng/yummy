// Package server 装配正式产品控制面模块；业务服务不依赖具体 HTTP 框架。
package server

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sunshanpeng/yummy/internal/platform/application"
	"github.com/sunshanpeng/yummy/internal/platform/audit"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/build"
	"github.com/sunshanpeng/yummy/internal/platform/buildsecret"
	"github.com/sunshanpeng/yummy/internal/platform/deployment"
	"github.com/sunshanpeng/yummy/internal/platform/enrollment"
	"github.com/sunshanpeng/yummy/internal/platform/identity"
	"github.com/sunshanpeng/yummy/internal/platform/inventory"
	"github.com/sunshanpeng/yummy/internal/platform/mcp"
	"github.com/sunshanpeng/yummy/internal/platform/oauth"
	"github.com/sunshanpeng/yummy/internal/platform/qualification"
	"github.com/sunshanpeng/yummy/internal/platform/runtimeconfig"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"github.com/sunshanpeng/yummy/internal/platform/session"
	"github.com/sunshanpeng/yummy/internal/platform/source"
	"github.com/sunshanpeng/yummy/internal/platform/store"
	"github.com/sunshanpeng/yummy/internal/platform/transport"
	"github.com/sunshanpeng/yummy/internal/platform/workergateway"
	"github.com/sunshanpeng/yummy/internal/platform/workspace"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type Config struct {
	DatabaseURL             string
	RedisAddress            string
	PublicBaseURL           string
	SessionKey              string
	CSRFKey                 string
	MasterKey               string
	OAuthKey                string
	SourceRoot              string
	WorkerCACertificatePath string
	WorkerCAPrivateKeyPath  string
	RegistryAuthHosts       map[string][]string
	RegistryDeniedHosts     []string
	AllowLocalHTTP          bool
	BehindTLSProxy          bool
}

type Server struct {
	Handler          http.Handler
	TransferHandler  http.Handler
	WorkerHandler    http.Handler
	WorkerGateway    *workergateway.Gateway
	lifecycle        *lifecycle
	Sources          *source.Service
	Inventory        *inventory.Service
	Builds           *build.Service
	BuildSecrets     *buildsecret.Service
	RuntimeConfigs   *runtimeconfig.Service
	Deployments      *deployment.Service
	Enrollment       *enrollment.Service
	OAuth            *oauth.Service
	artifactVerifier buildArtifactVerifier
	DB               *gorm.DB
	Identity         *identity.Service
	Access           *authorization.Access
	Sessions         *session.Manager
	Log              *zap.Logger
}

func Open(ctx context.Context, config Config, assets fs.FS) (*Server, error) {
	db, err := store.Open(ctx, config.DatabaseURL)
	if err != nil {
		return nil, err
	}
	return openWithDatabase(ctx, db, config, assets)
}

// openWithDatabase 与正式入口共享完整装配，测试只替换随机隔离schema，不替换业务服务。
func openWithDatabase(ctx context.Context, db *gorm.DB, config Config, assets fs.FS) (*Server, error) {
	log, err := zap.NewProduction()
	if err != nil {
		_ = store.Close(db)
		return nil, err
	}
	access, err := authorization.NewAccess(db)
	if err != nil {
		_ = store.Close(db)
		return nil, err
	}
	box, err := security.NewSecretBox(config.MasterKey)
	if err != nil {
		_ = store.Close(db)
		return nil, err
	}
	if config.MasterKey == config.SessionKey || config.MasterKey == config.CSRFKey {
		_ = store.Close(db)
		return nil, errors.New("主密钥不能与会话或CSRF密钥复用")
	}
	identityService, err := identity.NewService(db, access)
	if err != nil {
		_ = store.Close(db)
		return nil, err
	}
	var oauthService *oauth.Service
	var authenticateOAuth session.BearerAuthenticator
	if config.OAuthKey != "" {
		oauthKey, decodeErr := base64.StdEncoding.DecodeString(config.OAuthKey)
		if decodeErr != nil || len(oauthKey) != 32 || config.OAuthKey == config.MasterKey || config.OAuthKey == config.SessionKey || config.OAuthKey == config.CSRFKey {
			_ = store.Close(db)
			return nil, errors.New("OAuth 密钥必须是独立的 Base64 编码32字节随机值")
		}
		oauthService, err = oauth.NewService(db, access, identityService, oauth.Config{PublicBaseURL: config.PublicBaseURL, HMACSecret: oauthKey, AllowInsecureLoopback: config.AllowLocalHTTP})
		if err != nil {
			_ = store.Close(db)
			return nil, err
		}
		access.SetOAuthVerifier(oauthService.VerifyPrincipal)
		authenticateOAuth = oauthService.Authenticate
	}
	sessions, err := session.New(session.Config{RedisAddress: config.RedisAddress, PublicBaseURL: config.PublicBaseURL, CookieKey: config.SessionKey, CSRFKey: config.CSRFKey, AllowLocalHTTP: config.AllowLocalHTTP, BehindTLSProxy: config.BehindTLSProxy, OAuth: authenticateOAuth}, identityService, access)
	if err != nil {
		_ = store.Close(db)
		return nil, err
	}
	sources, err := source.NewService(db, access, source.Config{Root: config.SourceRoot})
	if err != nil {
		_ = sessions.Close()
		_ = store.Close(db)
		return nil, err
	}
	inventoryService := inventory.NewService(db, access, box)
	configService := runtimeconfig.NewService(db, access, box)
	applicationService := application.NewService(db, access)
	secretService := buildsecret.NewService(db, access, box)
	buildService := build.NewService(db, access, inventoryService)
	buildService.SetSecretResolver(secretService)
	reader := &artifactReader{db: db, inventory: inventoryService, authHosts: config.RegistryAuthHosts, deniedHosts: config.RegistryDeniedHosts}
	buildVerifier := buildArtifactVerifier{reader: reader}
	deploymentService := deployment.NewService(db, access, configService, deploymentArtifactVerifier{reader: reader})
	materials := &workerMaterials{db: db, box: box, source: sources, secrets: secretService, inventory: inventoryService, baseURL: strings.TrimRight(config.PublicBaseURL, "/")}
	var enrollmentService *enrollment.Service
	var gateway *workergateway.Gateway
	var workerHTTP http.Handler
	if config.WorkerCACertificatePath != "" || config.WorkerCAPrivateKeyPath != "" {
		enrollmentService, err = enrollment.NewService(db, access, enrollment.Config{CACertificatePath: config.WorkerCACertificatePath, CAPrivateKeyPath: config.WorkerCAPrivateKeyPath, OnRevoke: func(identity enrollment.Identity) {
			if gateway != nil {
				gateway.Revoke(identity.NodeID, identity.Serial)
			}
		}})
		if err != nil {
			_ = sources.Close()
			_ = sessions.Close()
			_ = store.Close(db)
			return nil, err
		}
		gateway, err = newWorkerGateway(workergateway.Config{DB: db, Peers: enrollmentService, Builds: buildService, BuildMaterial: materials, Deployments: deploymentService, DeploymentMaterial: materials, Guard: workerGenerationGuard{service: deploymentService}, Capabilities: qualification.NewVerifier(db), OnError: func(error) { log.Error("worker 网关调度或状态更新失败") }})
		if err != nil {
			_ = sources.Close()
			_ = sessions.Close()
			_ = store.Close(db)
			return nil, err
		}
		workerHTTP = workerHandler(enrollmentService, gateway)
	}
	router := gin.New()
	router.Use(gin.CustomRecovery(func(c *gin.Context, _ any) {
		log.Error("控制面请求发生异常")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"code": "internal", "message": "控制面内部错误"})
	}))
	// 不启用默认访问日志，防止token查询串、Cookie或请求正文意外写入日志。
	mount := func(path string, handler http.Handler) {
		router.Any("/api"+path+"*method", gin.WrapH(http.StripPrefix("/api", handler)))
	}
	mount(transport.NewIdentityHandler(identityService, sessions, log))
	mount(transport.NewWorkspaceHandler(workspace.NewService(db, access), log))
	mount(transport.NewApplicationHandler(applicationService, log))
	mount(transport.NewRuntimeConfigHandler(configService, log))
	mount(transport.NewSourceHandler(sources, log))
	mount(transport.NewInventoryHandler(inventoryService, log))
	mount(transport.NewBuildHandler(buildService, log))
	mount(transport.NewBuildSecretHandler(secretService, log))
	mount(transport.NewDeploymentHandler(deploymentService, log))
	mount(transport.NewEnrollmentHandler(enrollmentService, log))
	if oauthService != nil {
		// 全局管理员专用 OAuth 客户端管理；oauthService 未装配（未配置 OAuthKey）时失败关闭不注册。
		mount(transport.NewOAuthAdminHandler(oauthService, log))
	}
	router.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	var remoteMCP http.Handler
	if oauthService != nil {
		router.GET("/.well-known/oauth-authorization-server", gin.WrapF(oauthService.Metadata))
		router.GET("/oauth/authorize", gin.WrapF(oauthService.Authorize))
		router.GET("/oauth/consent", gin.WrapF(oauthService.Consent))
		router.POST("/oauth/consent", gin.WrapF(consentHandler(oauthService)))
		router.GET("/oauth/connections", gin.WrapF(connectionsHandler(oauthService)))
		router.POST("/oauth/connections/revoke", gin.WrapF(connectionsHandler(oauthService)))
		remoteMCP, err = mcp.NewHTTPHandler(mcp.Config{PublicBaseURL: config.PublicBaseURL, Authenticate: oauthService.Authenticate, AllowInsecureLoopback: config.AllowLocalHTTP}, mcp.Services{Applications: applicationService, RuntimeConfigs: configService, Sources: sources, Builds: buildService, Deployments: deploymentService})
		if err != nil {
			if gateway != nil {
				gateway.Close()
			}
			_ = sources.Close()
			_ = sessions.Close()
			_ = store.Close(db)
			return nil, err
		}
	}
	if assets != nil {
		files := http.FileServer(http.FS(assets))
		router.NoRoute(func(c *gin.Context) {
			if c.Request.Method != http.MethodGet || strings.HasPrefix(c.Request.URL.Path, "/api/") {
				c.Status(http.StatusNotFound)
				return
			}
			path := strings.TrimPrefix(c.Request.URL.Path, "/")
			if info, err := fs.Stat(assets, path); err == nil && !info.IsDir() {
				files.ServeHTTP(c.Writer, c.Request)
				return
			}
			index, err := fs.ReadFile(assets, "index.html")
			if err != nil {
				c.Status(http.StatusNotFound)
				return
			}
			c.Data(http.StatusOK, "text/html; charset=utf-8", index)
		})
	}
	protected := sessions.Middleware(router)
	// token/revoke 为无 Cookie 的 OAuth 协议端点；MCP 自行验证每次请求的应用受众。
	// 同意页仍通过会话、同源与 CSRF 中间件，不允许协议端点借用浏览器身份。
	entry := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if oauthService != nil && r.Host == strings.TrimPrefix(strings.TrimPrefix(strings.TrimRight(config.PublicBaseURL, "/"), "https://"), "http://") {
			switch r.URL.Path {
			case "/oauth/token":
				oauthService.Token(w, r)
				return
			case "/oauth/revoke":
				oauthService.Revoke(w, r)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/mcp/apps/") || strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource/mcp/apps/") {
				remoteMCP.ServeHTTP(w, r)
				return
			}
		}
		protected.ServeHTTP(w, r)
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := security.NewID("req")
		if err != nil {
			http.Error(w, "无法生成请求编号", http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		entry.ServeHTTP(w, r.WithContext(audit.WithRequestID(r.Context(), id)))
	})
	userTransfer := sessions.Middleware(transport.TransferHandler(sources, log))
	buildTransfer := transport.BuildGrantHandler(sources)
	transfer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/transfer/build-grants/") {
			buildTransfer.ServeHTTP(w, r)
			return
		}
		userTransfer.ServeHTTP(w, r)
	})
	product := &Server{Handler: handler, TransferHandler: transfer, WorkerHandler: workerHTTP, WorkerGateway: gateway, Sources: sources, Inventory: inventoryService, Builds: buildService, BuildSecrets: secretService, RuntimeConfigs: configService, Deployments: deploymentService, Enrollment: enrollmentService, OAuth: oauthService, artifactVerifier: buildVerifier, DB: db, Identity: identityService, Access: access, Sessions: sessions, Log: log}
	// 请求外后台循环与 Server 同生命周期；Close 时先停循环再释放数据库与会话。
	product.lifecycle = product.startLifecycle()
	return product, nil
}

func (s *Server) Close() error {
	s.lifecycle.Close()
	if s.WorkerGateway != nil {
		s.WorkerGateway.Close()
		s.WorkerGateway.Wait()
	}
	_ = s.Sources.Close()
	err := s.Sessions.Close()
	dbErr := store.Close(s.DB)
	_ = s.Log.Sync()
	if err != nil {
		return err
	}
	return dbErr
}
