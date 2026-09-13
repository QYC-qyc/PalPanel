package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"palpanel/internal/auth"
	"palpanel/internal/instance"
)

func (d Deps) registerInstances(v1 *gin.RouterGroup) {
	// 路径参数刻意用 :id：requirePerm 会把 :id 当实例 ID 做实例级权限判定，
	// 实例的 GET/PATCH 需要校验该用户对此实例是否有 grant。
	g := v1.Group("/instances", d.authRequired())
	g.GET("", d.requirePerm(auth.PInstanceRead), d.handleInstanceList)
	g.POST("", d.requirePerm(auth.PInstanceManage), d.handleInstanceCreate)
	g.GET("/:id", d.requirePerm(auth.PInstanceRead), d.handleInstanceGet)
	g.PATCH("/:id", d.requirePerm(auth.PInstanceUpdate), d.handleInstancePatch)
	g.DELETE("/:id", d.requirePerm(auth.PInstanceManage), d.handleInstanceDelete)
}

type instanceDTO struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	GameDir     string `json:"game_dir"`
	BackupDir   string `json:"backup_dir"`
	Status      string `json:"status"`
	GamePort    int    `json:"game_port"`
	QueryPort   int    `json:"query_port"`
	RestPort    int    `json:"rest_port"`
	RestEnabled bool   `json:"rest_enabled"`
	RconEnabled bool   `json:"rcon_enabled"`
	Autostart   bool   `json:"autostart"`
}

// toDTO 输出脱敏字段，永不包含 admin_password_enc。
func toDTO(in instance.Instance) instanceDTO {
	return instanceDTO{ID: in.ID, Name: in.Name, GameDir: in.GameDir, BackupDir: in.BackupDir,
		Status: in.Status, GamePort: in.GamePort, QueryPort: in.QueryPort, RestPort: in.RestPort,
		RestEnabled: in.RestEnabled, RconEnabled: in.RconEnabled, Autostart: in.Autostart}
}

// visibleInstances：有 instance.manage（全局）→ 全部；否则仅返回有 grant 的实例。
func (d Deps) visibleInstances(u auth.SessionUser) ([]instance.Instance, error) {
	all, err := d.Instances.List()
	if err != nil {
		return nil, err
	}
	// List() 已完全返回并关闭 rows，这里再调 Can 发起查询是安全的（MaxOpenConns(1)）
	ok, err := d.Auth.Can(u.ID, auth.PInstanceManage, 0)
	if err != nil {
		return nil, err
	}
	if ok {
		return all, nil
	}
	var out []instance.Instance
	for _, in := range all {
		g, err := d.Auth.Can(u.ID, auth.PInstanceRead, in.ID)
		if err != nil {
			return nil, err
		}
		if g {
			out = append(out, in)
		}
	}
	return out, nil
}

func (d Deps) handleInstanceList(c *gin.Context) {
	u := c.MustGet("user").(auth.SessionUser)
	list, err := d.visibleInstances(u)
	if err != nil {
		fail(c, http.StatusInternalServerError, "查询失败")
		return
	}
	out := []instanceDTO{}
	for _, in := range list {
		out = append(out, toDTO(in))
	}
	c.JSON(http.StatusOK, gin.H{"instances": out})
}

func (d Deps) handleInstanceGet(c *gin.Context) {
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"instance": toDTO(in)})
}

type instanceWriteReq struct {
	Name          string `json:"name" binding:"required,min=1"`
	GameDir       string `json:"game_dir" binding:"required"`
	BackupDir     string `json:"backup_dir"`
	GamePort      int    `json:"game_port"`
	QueryPort     int    `json:"query_port"`
	RestPort      *int   `json:"rest_port"` // 可选，0 或不传表示与 game_port 相同
	AdminPassword string `json:"admin_password" binding:"required,min=8"`
	RestEnabled   *bool  `json:"rest_enabled"`
	RconEnabled   *bool  `json:"rcon_enabled"`
	Autostart     bool   `json:"autostart"`
}

// createFromReq 组装待建实例：校验端口、加密密码。失败时已写响应并返回 false。
func (d Deps) createFromReq(c *gin.Context, req *instanceWriteReq) (instance.Instance, bool) {
	in := instance.Instance{
		Name: req.Name, GameDir: req.GameDir, BackupDir: req.BackupDir,
		GamePort: req.GamePort, QueryPort: req.QueryPort,
		RestEnabled: true, RconEnabled: true, Autostart: req.Autostart,
	}
	if req.RestEnabled != nil {
		in.RestEnabled = *req.RestEnabled
	}
	if req.RconEnabled != nil {
		in.RconEnabled = *req.RconEnabled
	}
	if req.RestPort != nil && *req.RestPort != 0 {
		in.RestPort = *req.RestPort
	}
	if in.GamePort <= 0 || in.GamePort > 65535 || in.QueryPort <= 0 || in.QueryPort > 65535 {
		fail(c, http.StatusBadRequest, "端口不合法")
		return in, false
	}
	if in.RestPort == 0 {
		in.RestPort = in.GamePort
	}
	if in.RestPort < 0 || in.RestPort > 65535 {
		fail(c, http.StatusBadRequest, "端口不合法")
		return in, false
	}
	enc, err := auth.Encrypt(d.Secret, []byte(req.AdminPassword))
	if err != nil {
		fail(c, http.StatusInternalServerError, "密码加密失败")
		return in, false
	}
	in.AdminPasswordEnc = enc
	return in, true
}

func (d Deps) handleInstanceCreate(c *gin.Context) {
	var req instanceWriteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数不合法：name/game_dir/admin_password 必填")
		return
	}
	in, ok := d.createFromReq(c, &req)
	if !ok {
		return
	}
	id, err := d.Instances.Create(in)
	if errors.Is(err, instance.ErrPortConflict) {
		fail(c, http.StatusConflict, "端口与现有实例冲突")
		return
	}
	if err != nil {
		fail(c, http.StatusInternalServerError, "创建失败")
		return
	}
	caller := c.MustGet("user").(auth.SessionUser)
	d.Audit.Record(caller.ID, caller.Username, id, "instance.create", "创建实例 "+in.Name, c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"id": id})
}

func (d Deps) handleInstancePatch(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	in, err := d.Instances.Get(id)
	if errors.Is(err, instance.ErrNotFound) {
		fail(c, http.StatusNotFound, "实例不存在")
		return
	}
	if err != nil {
		fail(c, http.StatusInternalServerError, "查询失败")
		return
	}
	var req struct {
		Name          *string `json:"name"`
		BackupDir     *string `json:"backup_dir"`
		GamePort      *int    `json:"game_port"`
		QueryPort     *int    `json:"query_port"`
		RestPort      *int    `json:"rest_port"` // 0 表示与 game_port 相同
		AdminPassword *string `json:"admin_password"`
		RestEnabled   *bool   `json:"rest_enabled"`
		RconEnabled   *bool   `json:"rcon_enabled"`
		Autostart     *bool   `json:"autostart"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数不合法")
		return
	}
	if req.Name != nil && *req.Name != "" {
		in.Name = *req.Name
	}
	if req.BackupDir != nil {
		in.BackupDir = *req.BackupDir
	}
	if req.GamePort != nil {
		in.GamePort = *req.GamePort
	}
	if req.QueryPort != nil {
		in.QueryPort = *req.QueryPort
	}
	if req.RestPort != nil {
		if *req.RestPort == 0 {
			in.RestPort = in.GamePort
		} else {
			in.RestPort = *req.RestPort
		}
	}
	if req.RestEnabled != nil {
		in.RestEnabled = *req.RestEnabled
	}
	if req.RconEnabled != nil {
		in.RconEnabled = *req.RconEnabled
	}
	if req.Autostart != nil {
		in.Autostart = *req.Autostart
	}
	if in.GamePort <= 0 || in.GamePort > 65535 || in.QueryPort <= 0 || in.QueryPort > 65535 ||
		in.RestPort <= 0 || in.RestPort > 65535 {
		fail(c, http.StatusBadRequest, "端口不合法")
		return
	}
	if req.AdminPassword != nil && *req.AdminPassword != "" {
		if len(*req.AdminPassword) < 8 {
			fail(c, http.StatusBadRequest, "密码强度不足：至少 8 位")
			return
		}
		enc, err := auth.Encrypt(d.Secret, []byte(*req.AdminPassword))
		if err != nil {
			fail(c, http.StatusInternalServerError, "密码加密失败")
			return
		}
		in.AdminPasswordEnc = enc
	}
	if err := d.Instances.Update(in); errors.Is(err, instance.ErrPortConflict) {
		fail(c, http.StatusConflict, "端口与现有实例冲突")
		return
	} else if err != nil {
		fail(c, http.StatusInternalServerError, "保存失败")
		return
	}
	caller := c.MustGet("user").(auth.SessionUser)
	d.Audit.Record(caller.ID, caller.Username, id, "instance.update", "修改实例 "+in.Name, c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (d Deps) handleInstanceDelete(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if err := d.Instances.Delete(id); errors.Is(err, instance.ErrNotFound) {
		fail(c, http.StatusNotFound, "实例不存在")
		return
	} else if err != nil {
		fail(c, http.StatusInternalServerError, "删除失败")
		return
	}
	// 不使用数据库外键约束，级联由应用层负责：实例删除后清理其授权行，避免孤儿 grant。
	// Instances.Delete 内部已提交、无法纳入同一事务，故两步顺序执行；第二步失败返回 500，
	// 此时实例已删但授权行残留，只能靠运维清理（现实中仅在 DB 故障时发生）。
	if _, err := d.DB.Exec(`DELETE FROM instance_grants WHERE instance_id=?`, id); err != nil {
		fail(c, http.StatusInternalServerError, "删除失败")
		return
	}
	caller := c.MustGet("user").(auth.SessionUser)
	d.Audit.Record(caller.ID, caller.Username, id, "instance.delete", "删除实例", c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
