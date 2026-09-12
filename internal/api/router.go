package api

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"

	"palpanel/internal/config"
)

type Deps struct {
	Cfg config.Config
	DB  *sql.DB // Task 2 接入
}

func New(d Deps) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	v1 := r.Group("/api/v1")
	v1.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return r
}

func Run(cfg config.Config) error {
	return New(Deps{Cfg: cfg}).Run(cfg.Listen)
}
