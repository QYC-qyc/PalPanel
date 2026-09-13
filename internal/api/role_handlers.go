package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"palpanel/internal/auth"
)

func (d Deps) registerRoles(v1 *gin.RouterGroup) {
	// 路径参数用 :rid 而非 :id：requirePerm 只把 :id 当实例 ID 做实例级判定，
	// 角色管理是全局权限，不应触发实例 grant 校验。
	g := v1.Group("/roles", d.authRequired(), d.requirePerm(auth.PRoleManage))
	g.GET("", d.handleRoleList)
	g.POST("", d.handleRoleCreate)
	g.PUT("/:rid/permissions", d.handleRolePerms)
	g.DELETE("/:rid", d.handleRoleDelete)
}

type roleDTO struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	IsBuiltin   bool     `json:"is_builtin"`
	Permissions []string `json:"permissions"`
}

func (d Deps) handleRoleList(c *gin.Context) {
	rows, err := d.DB.Query(`SELECT id, name, description, is_builtin FROM roles ORDER BY id`)
	if err != nil {
		fail(c, http.StatusInternalServerError, "查询失败")
		return
	}
	// defer Close：Scan/Err 失败的 return 路径也必须释放连接（MaxOpenConns(1) 下泄漏即全服阻塞）。
	// database/sql 的 Close 幂等，正常读完后再显式关闭也安全。
	defer rows.Close()
	// DB 连接池为 MaxOpenConns(1)：必须先读完角色行并关闭 rows，
	// 才能再发起权限码查询，否则内层 Query 会等连接造成死锁。
	out := []roleDTO{}
	for rows.Next() {
		var r roleDTO
		var builtin int
		if err := rows.Scan(&r.ID, &r.Name, &r.Description, &builtin); err != nil {
			fail(c, http.StatusInternalServerError, "查询失败")
			return
		}
		r.IsBuiltin = builtin == 1
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		fail(c, http.StatusInternalServerError, "查询失败")
		return
	}
	for i := range out {
		prows, err := d.DB.Query(`SELECT code FROM role_permissions WHERE role_id=?`, out[i].ID)
		if err != nil {
			fail(c, http.StatusInternalServerError, "查询失败")
			return
		}
		for prows.Next() {
			var code string
			if err := prows.Scan(&code); err != nil {
				prows.Close()
				fail(c, http.StatusInternalServerError, "查询失败")
				return
			}
			out[i].Permissions = append(out[i].Permissions, code)
		}
		if err := prows.Err(); err != nil {
			prows.Close()
			fail(c, http.StatusInternalServerError, "查询失败")
			return
		}
		prows.Close()
	}
	c.JSON(http.StatusOK, gin.H{"roles": out})
}

func (d Deps) handleRoleCreate(c *gin.Context) {
	var req struct {
		Name        string   `json:"name" binding:"required,min=2"`
		Description string   `json:"description"`
		Permissions []string `json:"permissions"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数不合法")
		return
	}
	if err := d.validatePermCodes(req.Permissions); err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	res, err := d.DB.Exec(`INSERT INTO roles(name, description, is_builtin) VALUES(?,?,0)`, req.Name, req.Description)
	if err != nil {
		fail(c, http.StatusConflict, "角色名已存在")
		return
	}
	rid, _ := res.LastInsertId()
	if err := d.Auth.ReplaceRolePermissions(rid, req.Permissions); err != nil {
		fail(c, http.StatusInternalServerError, "保存失败")
		return
	}
	caller := c.MustGet("user").(auth.SessionUser)
	d.Audit.Record(caller.ID, caller.Username, 0, "role.create", "创建角色 "+req.Name, c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"id": rid})
}

func (d Deps) handleRolePerms(c *gin.Context) {
	rid, err := strconv.ParseInt(c.Param("rid"), 10, 64)
	if err != nil || rid <= 0 {
		fail(c, http.StatusBadRequest, "角色 ID 不合法")
		return
	}
	var req struct {
		Permissions []string `json:"permissions" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数不合法")
		return
	}
	if err := d.validatePermCodes(req.Permissions); err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	var builtin int
	if err := d.DB.QueryRow(`SELECT is_builtin FROM roles WHERE id=?`, rid).Scan(&builtin); err != nil {
		fail(c, http.StatusNotFound, "角色不存在")
		return
	}
	if builtin == 1 {
		// 内置角色允许调权限（operator/viewer 可微调），但 admin 不允许自废
		var adminID int64
		if err := d.DB.QueryRow(`SELECT id FROM roles WHERE name='admin'`).Scan(&adminID); err != nil {
			fail(c, http.StatusInternalServerError, "保存失败")
			return
		}
		if rid == adminID {
			fail(c, http.StatusBadRequest, "不能修改 admin 角色的权限")
			return
		}
	}
	if err := d.Auth.ReplaceRolePermissions(rid, req.Permissions); err != nil {
		fail(c, http.StatusInternalServerError, "保存失败")
		return
	}
	caller := c.MustGet("user").(auth.SessionUser)
	d.Audit.Record(caller.ID, caller.Username, 0, "role.perms", "调整角色权限", c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (d Deps) handleRoleDelete(c *gin.Context) {
	rid, err := strconv.ParseInt(c.Param("rid"), 10, 64)
	if err != nil || rid <= 0 {
		fail(c, http.StatusBadRequest, "角色 ID 不合法")
		return
	}
	var builtin int
	if err := d.DB.QueryRow(`SELECT is_builtin FROM roles WHERE id=?`, rid).Scan(&builtin); err != nil {
		fail(c, http.StatusNotFound, "角色不存在")
		return
	}
	if builtin == 1 {
		fail(c, http.StatusBadRequest, "内置角色不可删除")
		return
	}
	// 4 表删除放进一个事务：任一失败整体回滚，不留半删状态（无外键约束，级联靠应用层）。
	tx, err := d.DB.Begin()
	if err != nil {
		fail(c, http.StatusInternalServerError, "删除失败")
		return
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM roles WHERE id=?`,
		`DELETE FROM role_permissions WHERE role_id=?`,
		`DELETE FROM user_roles WHERE role_id=?`,
		`DELETE FROM instance_grants WHERE role_id=?`,
	} {
		if _, err := tx.Exec(stmt, rid); err != nil {
			fail(c, http.StatusInternalServerError, "删除失败")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fail(c, http.StatusInternalServerError, "删除失败")
		return
	}
	caller := c.MustGet("user").(auth.SessionUser)
	d.Audit.Record(caller.ID, caller.Username, 0, "role.delete", "删除角色", c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (d Deps) validatePermCodes(codes []string) error {
	valid := map[string]bool{}
	for _, p := range auth.AllPermissions {
		valid[p] = true
	}
	for _, code := range codes {
		if !valid[code] {
			return errors.New("未知权限码: " + code)
		}
	}
	return nil
}
