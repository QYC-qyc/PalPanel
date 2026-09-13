package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"palpanel/internal/auth"
)

func (d Deps) authRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			fail(c, http.StatusUnauthorized, "未登录")
			c.Abort()
			return
		}
		uid, rv, err := auth.ParseToken(d.Auth.Secret, strings.TrimPrefix(h, "Bearer "))
		if err != nil {
			fail(c, http.StatusUnauthorized, "登录已失效")
			c.Abort()
			return
		}
		u, err := d.Auth.SessionUser(uid, rv)
		if err != nil {
			fail(c, http.StatusUnauthorized, "登录已失效，请重新登录")
			c.Abort()
			return
		}
		c.Set("uid", u.ID)
		c.Set("username", u.Username)
		c.Set("user", u)
		c.Next()
	}
}

// requirePerm：进入 handler 前判定权限。路径含 :id 时按实例级权限判定。
func (d Deps) requirePerm(code string) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := c.MustGet("user").(auth.SessionUser)
		var instanceID int64
		if v := c.Param("id"); v != "" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				fail(c, http.StatusBadRequest, "实例 ID 不合法")
				c.Abort()
				return
			}
			instanceID = id
		}
		ok, err := d.Auth.Can(u.ID, code, instanceID)
		if err != nil {
			fail(c, http.StatusInternalServerError, "权限判定失败")
			c.Abort()
			return
		}
		if !ok {
			fail(c, http.StatusForbidden, "没有执行该操作的权限")
			c.Abort()
			return
		}
		c.Next()
	}
}
