package api

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"

	"palpanel/internal/audit"
	"palpanel/internal/auth"
	"palpanel/internal/config"
	"palpanel/internal/event"
	"palpanel/internal/gateway"
	"palpanel/internal/instance"
	"palpanel/internal/job"
	"palpanel/internal/rcon"
	"palpanel/internal/supervisor"
)

type Deps struct {
	Cfg       config.Config
	DB        *sql.DB // Task 2 接入
	Auth      *auth.Service
	Audit     *audit.Recorder
	Secret    []byte
	Instances *instance.Store
	Hub       *event.Hub   // Task 3 WS 推送
	Jobs      *job.Manager // Task 4+ 任务调度
	Sup       *supervisor.Manager // Task 9 进程守护（StartFn/Killer 由 main 或测试注入）

	// RESTFor/RCONFor 是生产与测试共用的注入点：按实例取 REST/RCON 客户端。
	// 生产实现 = DefaultRESTFor/DefaultRCONFor(secret)（解密 AdminPassword → 本机连接）；
	// StopHooks、players/announce/save/status-metrics 全部经这两个入口取 client。
	RESTFor func(instance.Instance) (*gateway.Client, error)
	RCONFor func(instance.Instance) (*rcon.Conn, error)

	// Installer 安装/更新服务（生产为 *installer.Service，测试可注入 fake）。
	Installer Installer
}

func New(d Deps) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/api/ws", d.handleWS) // 独立于 /api/v1：不走鉴权中间件，handler 内 token 鉴权
	v1 := r.Group("/api/v1")
	v1.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	d.registerAuth(v1)
	d.registerAuthed(v1)
	d.registerUsers(v1)
	d.registerRoles(v1)
	d.registerInstances(v1)
	d.registerInstanceActions(v1)
	d.registerAudit(v1)
	return r
}
