package api

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"

	"palpanel/internal/audit"
	"palpanel/internal/auth"
	"palpanel/internal/config"
)

type Deps struct {
	Cfg    config.Config
	DB     *sql.DB // Task 2 接入
	Auth   *auth.Service
	Audit  *audit.Recorder
	Secret []byte
}

func New(d Deps) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	v1 := r.Group("/api/v1")
	v1.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	d.registerAuth(v1)
	d.registerAuthed(v1)
	d.registerUsers(v1)
	d.registerRoles(v1)
	return r
}

func Run(cfg config.Config) error {
	return New(Deps{Cfg: cfg}).Run(cfg.Listen)
}
