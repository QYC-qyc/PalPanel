package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"golang.org/x/crypto/bcrypt"

	"palpanel/internal/auth"
)

func (d Deps) registerUsers(v1 *gin.RouterGroup) {
	// 路径参数用 :uid 而非 :id：requirePerm 只把 :id 当实例 ID 做实例级判定，
	// 用户管理是全局权限，不应触发实例 grant 校验。
	g := v1.Group("/users", d.authRequired(), d.requirePerm(auth.PUserManage))
	g.GET("", d.handleUserList)
	g.POST("", d.handleUserCreate)
	g.PATCH("/:uid", d.handleUserPatch)
	g.PUT("/:uid/roles", d.handleUserRoles)
	g.PUT("/:uid/grants", d.handleUserGrants)
	g.DELETE("/:uid", d.handleUserDelete)
}

type userDTO struct {
	ID          int64    `json:"id"`
	Username    string   `json:"username"`
	DisplayName string   `json:"display_name"`
	IsActive    bool     `json:"is_active"`
	Roles       []string `json:"roles"`
}

func (d Deps) userByID(id int64) (userDTO, error) {
	u := userDTO{Roles: []string{}}
	err := d.DB.QueryRow(`SELECT id, username, display_name, is_active FROM users WHERE id=?`, id).
		Scan(&u.ID, &u.Username, &u.DisplayName, &u.IsActive)
	if err != nil {
		return u, err
	}
	rows, err := d.DB.Query(`SELECT r.name FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=?`, id)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return u, err
		}
		u.Roles = append(u.Roles, n)
	}
	return u, rows.Err()
}

func (d Deps) handleUserList(c *gin.Context) {
	rows, err := d.DB.Query(`SELECT id FROM users ORDER BY id`)
	if err != nil {
		fail(c, http.StatusInternalServerError, "查询失败")
		return
	}
	// DB 连接池为 MaxOpenConns(1)：必须先读完用户行并关闭 rows，
	// 才能再调用 userByID 发起内层查询，否则内层 Query 会等连接造成死锁。
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			fail(c, http.StatusInternalServerError, "查询失败")
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		fail(c, http.StatusInternalServerError, "查询失败")
		return
	}
	rows.Close()
	out := []userDTO{}
	for _, id := range ids {
		u, err := d.userByID(id)
		if err != nil {
			fail(c, http.StatusInternalServerError, "查询失败")
			return
		}
		out = append(out, u)
	}
	c.JSON(http.StatusOK, gin.H{"users": out})
}

func (d Deps) handleUserCreate(c *gin.Context) {
	var req struct {
		Username    string  `json:"username" binding:"required,min=2"`
		Password    string  `json:"password" binding:"required"`
		DisplayName string  `json:"display_name"`
		RoleIDs     []int64 `json:"role_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数不合法")
		return
	}
	if len(req.Password) < 8 {
		fail(c, http.StatusBadRequest, "密码强度不足：至少 8 位")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(c, http.StatusInternalServerError, "创建失败")
		return
	}
	res, err := d.DB.Exec(`INSERT INTO users(username, display_name, password_hash) VALUES(?,?,?)`,
		req.Username, req.DisplayName, string(hash))
	if err != nil {
		fail(c, http.StatusConflict, "用户名已存在")
		return
	}
	uid, _ := res.LastInsertId()
	caller := c.MustGet("user").(auth.SessionUser)
	if len(req.RoleIDs) > 0 {
		if err := d.Auth.ReplaceUserRoles(uid, req.RoleIDs); err != nil {
			fail(c, http.StatusInternalServerError, "角色设置失败")
			return
		}
	}
	d.Audit.Record(caller.ID, caller.Username, 0, "user.create", "创建用户 "+req.Username, c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"id": uid})
}

func (d Deps) handleUserPatch(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("uid"), 10, 64)
	if err != nil || id <= 0 {
		fail(c, http.StatusBadRequest, "用户 ID 不合法")
		return
	}
	var req struct {
		DisplayName *string `json:"display_name"`
		IsActive    *bool   `json:"is_active"`
		Password    *string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数不合法")
		return
	}
	if req.Password != nil {
		if len(*req.Password) < 8 {
			fail(c, http.StatusBadRequest, "密码强度不足：至少 8 位")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(*req.Password), bcrypt.DefaultCost)
		if err != nil {
			fail(c, http.StatusInternalServerError, "更新失败")
			return
		}
		if _, err := d.DB.Exec(`UPDATE users SET password_hash=? WHERE id=?`, string(hash), id); err != nil {
			fail(c, http.StatusInternalServerError, "更新失败")
			return
		}
	}
	if req.DisplayName != nil {
		if _, err := d.DB.Exec(`UPDATE users SET display_name=? WHERE id=?`, *req.DisplayName, id); err != nil {
			fail(c, http.StatusInternalServerError, "更新失败")
			return
		}
	}
	if req.IsActive != nil {
		if _, err := d.DB.Exec(`UPDATE users SET is_active=? WHERE id=?`, boolToInt(*req.IsActive), id); err != nil {
			fail(c, http.StatusInternalServerError, "更新失败")
			return
		}
	}
	if err := d.Auth.BumpUser(id); err != nil {
		fail(c, http.StatusInternalServerError, "更新失败")
		return
	}
	caller := c.MustGet("user").(auth.SessionUser)
	d.Audit.Record(caller.ID, caller.Username, 0, "user.update", "更新用户", c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (d Deps) handleUserRoles(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("uid"), 10, 64)
	if err != nil || id <= 0 {
		fail(c, http.StatusBadRequest, "用户 ID 不合法")
		return
	}
	var req struct {
		RoleIDs []int64 `json:"role_ids" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数不合法")
		return
	}
	if err := d.Auth.ReplaceUserRoles(id, req.RoleIDs); err != nil {
		fail(c, http.StatusInternalServerError, "角色设置失败")
		return
	}
	caller := c.MustGet("user").(auth.SessionUser)
	d.Audit.Record(caller.ID, caller.Username, 0, "user.roles", "调整用户角色", c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (d Deps) handleUserGrants(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("uid"), 10, 64)
	if err != nil || id <= 0 {
		fail(c, http.StatusBadRequest, "用户 ID 不合法")
		return
	}
	var req struct {
		InstanceIDs []int64 `json:"instance_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数不合法")
		return
	}
	if err := d.Auth.ReplaceUserGrants(id, req.InstanceIDs); err != nil {
		fail(c, http.StatusInternalServerError, "授权失败")
		return
	}
	caller := c.MustGet("user").(auth.SessionUser)
	d.Audit.Record(caller.ID, caller.Username, 0, "user.grants", "调整实例授权", c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (d Deps) handleUserDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("uid"), 10, 64)
	if err != nil || id <= 0 {
		fail(c, http.StatusBadRequest, "用户 ID 不合法")
		return
	}
	caller := c.MustGet("user").(auth.SessionUser)
	if id == caller.ID {
		fail(c, http.StatusBadRequest, "不能删除自己")
		return
	}
	// 一个事务内完成：last-admin 复查（事务内重读，防并发双删）→ 删 users → 级联清理 → 提交。
	tx, err := d.DB.Begin()
	if err != nil {
		fail(c, http.StatusInternalServerError, "删除失败")
		return
	}
	defer tx.Rollback()

	// ① 事务内重查：该用户是否 admin + admin 总数
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM user_roles ur JOIN roles r ON r.id=ur.role_id
		WHERE r.name='admin' AND ur.user_id=?`, id).Scan(&n); err != nil {
		fail(c, http.StatusInternalServerError, "删除失败")
		return
	}
	if n > 0 {
		var total int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE r.name='admin'`).Scan(&total); err != nil {
			fail(c, http.StatusInternalServerError, "删除失败")
			return
		}
		if total <= 1 {
			fail(c, http.StatusBadRequest, "不能删除最后一个管理员")
			return
		}
	}
	// ② 删 users
	res, err := tx.Exec(`DELETE FROM users WHERE id=?`, id)
	if err != nil {
		fail(c, http.StatusInternalServerError, "删除失败")
		return
	}
	affected, err := res.RowsAffected()
	if err != nil {
		fail(c, http.StatusInternalServerError, "删除失败")
		return
	}
	if affected == 0 {
		fail(c, http.StatusNotFound, "用户不存在")
		return
	}
	// ③ 级联清理（无外键约束，级联由应用层负责）
	if _, err := tx.Exec(`DELETE FROM user_roles WHERE user_id=?`, id); err != nil {
		fail(c, http.StatusInternalServerError, "删除失败")
		return
	}
	if _, err := tx.Exec(`DELETE FROM instance_grants WHERE user_id=?`, id); err != nil {
		fail(c, http.StatusInternalServerError, "删除失败")
		return
	}
	// ④ 提交
	if err := tx.Commit(); err != nil {
		fail(c, http.StatusInternalServerError, "删除失败")
		return
	}
	d.Audit.Record(caller.ID, caller.Username, 0, "user.delete", "删除用户", c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
