package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"palpanel/internal/auth"
)

func (d Deps) registerAudit(v1 *gin.RouterGroup) {
	v1.GET("/audit-logs", d.authRequired(), d.requirePerm(auth.PAuditRead), d.handleAuditList)
}

func (d Deps) handleAuditList(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	where := "WHERE 1=1"
	var args []any
	if v := c.Query("user_id"); v != "" {
		where += " AND user_id=?"
		args = append(args, v)
	}
	if v := c.Query("instance_id"); v != "" {
		where += " AND instance_id=?"
		args = append(args, v)
	}
	if v := c.Query("action"); v != "" {
		where += " AND action LIKE ?"
		args = append(args, v+"%")
	}
	var total int
	if err := d.DB.QueryRow(`SELECT COUNT(*) FROM audit_logs `+where, args...).Scan(&total); err != nil {
		fail(c, http.StatusInternalServerError, "查询失败")
		return
	}
	args = append(args, size, (page-1)*size)
	rows, err := d.DB.Query(`SELECT id, user_id, username, instance_id, action, detail, ip, created_at
		FROM audit_logs `+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		fail(c, http.StatusInternalServerError, "查询失败")
		return
	}
	defer rows.Close()
	type entry struct {
		ID         int64  `json:"id"`
		UserID     *int64 `json:"user_id"`
		Username   string `json:"username"`
		InstanceID *int64 `json:"instance_id"`
		Action     string `json:"action"`
		Detail     string `json:"detail"`
		IP         string `json:"ip"`
		CreatedAt  string `json:"created_at"`
	}
	var out []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ID, &e.UserID, &e.Username, &e.InstanceID, &e.Action, &e.Detail, &e.IP, &e.CreatedAt); err != nil {
			fail(c, http.StatusInternalServerError, "查询失败")
			return
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		fail(c, http.StatusInternalServerError, "查询失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "page": page, "page_size": size, "logs": out})
}
