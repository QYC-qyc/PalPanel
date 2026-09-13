package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"palpanel/internal/auth"
)

func fail(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"error": msg})
}

func (d Deps) registerAuth(v1 *gin.RouterGroup) {
	v1.POST("/setup", d.handleSetup)
}

func (d Deps) handleSetup(c *gin.Context) {
	var req struct {
		Username string `json:"username" binding:"required,min=3"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数不合法")
		return
	}
	uid, err := d.Auth.Setup(req.Username, req.Password)
	if errors.Is(err, auth.ErrSetupDone) {
		fail(c, http.StatusForbidden, err.Error())
		return
	}
	if errors.Is(err, auth.ErrWeakPassword) {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		fail(c, http.StatusInternalServerError, "初始化失败")
		return
	}
	d.Audit.Record(uid, req.Username, 0, "auth.setup", "初始化管理员", c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"id": uid})
}
