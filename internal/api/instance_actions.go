// 实例动作端点：启停/重启（进程守护）、安装/更新（异步 job）、状态/日志、
// 玩家管理（players/kick/ban）、announce/save（官方 REST 网关）。
//
// 注入点设计（生产与测试共用）：
//   - Deps.RESTFor / Deps.RCONFor：按实例取 REST/RCON 客户端（Default*For 解密密码）；
//   - Deps.Installer：安装/更新服务最小接口（生产 *installer.Service，测试 fake）；
//   - StopHooks 按实例配置在每次 start 前重建并注册到 supervisor（REST Shutdown → RCON Exit → 强杀）。
package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"palpanel/internal/auth"
	"palpanel/internal/backup"
	"palpanel/internal/gateway"
	"palpanel/internal/instance"
	"palpanel/internal/job"
	"palpanel/internal/rcon"
	"palpanel/internal/supervisor"
)

// ErrUpdateInProgress：更新任务运行中拒绝启动实例（handleInstanceStart 映射 409）。
var ErrUpdateInProgress = errors.New("更新进行中，暂不能启动")

// Installer 是安装/更新服务的最小接口（生产注入 *installer.Service，测试注入 fake）。
type Installer interface {
	Install(ctx context.Context, gameDir string, report func(progress int, message string), onLine func(string)) error
	UpdateCheck(ctx context.Context, gameDir string, onLine func(string)) (local, remote string, err error)
}

func (d Deps) registerInstanceActions(v1 *gin.RouterGroup) {
	g := v1.Group("/instances", d.authRequired())
	g.POST("/:id/start", d.requirePerm(auth.PInstanceStart), d.handleInstanceStart)
	g.POST("/:id/stop", d.requirePerm(auth.PInstanceStop), d.handleInstanceStop)
	// restart 需同时具备 start 与 stop 权限：两道 requirePerm 串联判定
	g.POST("/:id/restart", d.requirePerm(auth.PInstanceStart), d.requirePerm(auth.PInstanceStop), d.handleInstanceRestart)
	g.POST("/:id/install", d.requirePerm(auth.PInstanceInstall), d.handleInstanceInstall)
	g.GET("/:id/update-check", d.requirePerm(auth.PInstanceUpgrade), d.handleInstanceUpdateCheck)
	g.POST("/:id/update", d.requirePerm(auth.PInstanceUpgrade), d.handleInstanceUpdate)
	g.GET("/:id/status", d.requirePerm(auth.PInstanceRead), d.handleInstanceStatus)
	g.GET("/:id/logs", d.requirePerm(auth.PInstanceRead), d.handleInstanceLogs)
	g.GET("/:id/players", d.requirePerm(auth.PPlayerRead), d.handleInstancePlayers)
	g.POST("/:id/players/kick", d.requirePerm(auth.PPlayerKick), d.handlePlayerKick)
	g.POST("/:id/players/ban", d.requirePerm(auth.PPlayerBan), d.handlePlayerBan)
	g.POST("/:id/announce", d.requirePerm(auth.PPlayerAnnounce), d.handlePlayerAnnounce)
	g.POST("/:id/save", d.requirePerm(auth.PPlayerSave), d.handlePlayerSave)
}

// parseID 统一路径 :id 解析（此前各 handler 重复的 ParseInt 模式）。
// 失败时已写响应并返回 false。
func parseID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		fail(c, http.StatusBadRequest, "实例 ID 不合法")
		return 0, false
	}
	return id, true
}

// loadInstance：parseID + Get + 404。失败时已写响应并返回 false。
func (d Deps) loadInstance(c *gin.Context) (instance.Instance, bool) {
	id, ok := parseID(c)
	if !ok {
		return instance.Instance{}, false
	}
	in, err := d.Instances.Get(id)
	if errors.Is(err, instance.ErrNotFound) {
		fail(c, http.StatusNotFound, "实例不存在")
		return instance.Instance{}, false
	}
	if err != nil {
		fail(c, http.StatusInternalServerError, "查询失败")
		return instance.Instance{}, false
	}
	return in, true
}

// audit 统一审计记录。
func (d Deps) audit(c *gin.Context, instanceID int64, action, desc string) {
	caller := c.MustGet("user").(auth.SessionUser)
	d.Audit.Record(caller.ID, caller.Username, instanceID, action, desc, c.ClientIP())
}

// ---- 注入点默认实现（main 装配；测试替换为 fake） ----

// DefaultRESTFor 生产实现：rest_enabled 守卫 → 解密 AdminPassword → 本机 REST 客户端。
func DefaultRESTFor(secret []byte) func(instance.Instance) (*gateway.Client, error) {
	return func(inst instance.Instance) (*gateway.Client, error) {
		if !inst.RestEnabled {
			return nil, errors.New("该实例未启用 REST API")
		}
		pw, err := auth.Decrypt(secret, inst.AdminPasswordEnc)
		if err != nil {
			return nil, fmt.Errorf("解密管理密码失败: %w", err)
		}
		return gateway.New("127.0.0.1", inst.GamePort, string(pw)), nil
	}
}

// DefaultRCONFor 生产实现：rcon_enabled 守卫 → 解密 AdminPassword → RCON 拨号鉴权。
// 拨号与鉴权读均受 ctx 控制（本注入点无 ctx 入参，用 10s 超时兜底）。
func DefaultRCONFor(secret []byte) func(instance.Instance) (*rcon.Conn, error) {
	return func(inst instance.Instance) (*rcon.Conn, error) {
		if !inst.RconEnabled {
			return nil, errors.New("该实例未启用 RCON")
		}
		pw, err := auth.Decrypt(secret, inst.AdminPasswordEnc)
		if err != nil {
			return nil, fmt.Errorf("解密管理密码失败: %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return rcon.Dial(ctx, fmt.Sprintf("127.0.0.1:%d", inst.GamePort), string(pw))
	}
}

// restClient / rconClient：nil 安全的注入点访问。
func (d Deps) restClient(inst instance.Instance) (*gateway.Client, error) {
	if d.RESTFor == nil {
		return nil, errors.New("REST 未配置")
	}
	return d.RESTFor(inst)
}

func (d Deps) rconClient(inst instance.Instance) (*rcon.Conn, error) {
	if d.RCONFor == nil {
		return nil, errors.New("RCON 未配置")
	}
	return d.RCONFor(inst)
}

// ---- 优雅停机钩子（supervisor.StopHooks 由 API 层构造） ----

// stopHooks 按实例当前配置构造优雅停机链：REST Shutdown(30s)（rest_enabled 时）→
// RCON Exit（rcon_enabled 时）→ 两者皆缺/皆败由 supervisor 强杀兜底。
func (d Deps) stopHooks(inst instance.Instance) supervisor.StopHooks {
	var h supervisor.StopHooks
	if client, err := d.restClient(inst); err == nil {
		h.REST = func(ctx context.Context) error {
			return client.Shutdown(ctx, 30, "面板发起停机")
		}
	}
	if inst.RconEnabled && d.RCONFor != nil {
		h.RCON = func(ctx context.Context) error { return d.rconExit(ctx, inst) }
	}
	return h
}

// rconExit 经 RCON 注入点拨号鉴权后执行 Exit 并关闭。
// 拨号/鉴权读受 ctx 控制；Exec 尚无 ctx（内部 10s 读超时），故整体仍包一层
// goroutine+select：ctx 取消时本调用立即返回，底层 Exec 随读超时自行收尾。
func (d Deps) rconExit(ctx context.Context, inst instance.Instance) error {
	type res struct{ err error }
	ch := make(chan res, 1)
	go func() {
		conn, err := d.rconClient(inst)
		if err == nil {
			_, err = conn.Exec("Exit")
			_ = conn.Close()
		}
		ch <- res{err}
	}()
	select {
	case r := <-ch:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startSup 注册最新停机钩子后启动实例（钩子按实例当前配置每次重建，实例编辑后即刻生效）。
func (d Deps) startSup(id int64) error {
	inst, err := d.Instances.Get(id)
	if err != nil {
		if errors.Is(err, instance.ErrNotFound) {
			return supervisor.ErrNotFound
		}
		return err
	}
	if d.Sup == nil {
		return errors.New("进程守护未配置")
	}
	d.Sup.SetHooks(id, d.stopHooks(inst))
	return d.Sup.Start(id)
}

// StartInstance 供 main/autostart 使用：注册停机钩子后启动实例。
func (d Deps) StartInstance(id int64) error { return d.startSup(id) }

// ---- 启动/停止/重启 ----

func (d Deps) handleInstanceStart(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	// 互斥：更新 job 运行中不允许启动（steamcmd 正在覆写文件，进程拉起必然异常）
	if d.Jobs != nil && d.Jobs.RunningOfKind(id, "update") {
		fail(c, http.StatusConflict, ErrUpdateInProgress.Error())
		return
	}
	if err := d.startSup(id); errors.Is(err, supervisor.ErrAlreadyRunning) {
		fail(c, http.StatusConflict, err.Error())
		return
	} else if errors.Is(err, supervisor.ErrNotFound) {
		fail(c, http.StatusNotFound, "实例不存在")
		return
	} else if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	d.audit(c, id, "instance.start", "启动实例")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (d Deps) handleInstanceStop(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if d.Sup == nil {
		fail(c, http.StatusInternalServerError, "进程守护未配置")
		return
	}
	if err := d.Sup.Stop(id); errors.Is(err, supervisor.ErrNotRunning) {
		fail(c, http.StatusConflict, err.Error())
		return
	} else if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	d.audit(c, id, "instance.stop", "停止实例")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (d Deps) handleInstanceRestart(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if d.Sup == nil {
		fail(c, http.StatusInternalServerError, "进程守护未配置")
		return
	}
	// stop+start 序列：未运行时停服步骤幂等容忍，继续拉起
	if err := d.Sup.Stop(id); err != nil && !errors.Is(err, supervisor.ErrNotRunning) {
		fail(c, http.StatusInternalServerError, "停服失败: "+err.Error())
		return
	}
	if err := d.startSup(id); errors.Is(err, supervisor.ErrNotFound) {
		fail(c, http.StatusNotFound, "实例不存在")
		return
	} else if err != nil {
		fail(c, http.StatusInternalServerError, "重启失败: "+err.Error())
		return
	}
	d.audit(c, id, "instance.restart", "重启实例")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- 安装/更新（异步 job） ----

func (d Deps) handleInstanceInstall(c *gin.Context) {
	if d.Installer == nil || d.Jobs == nil {
		fail(c, http.StatusInternalServerError, "安装服务未配置")
		return
	}
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	if _, err := d.Jobs.Start("install", in.ID, func(ctx context.Context, report func(int, string)) error {
		return d.Installer.Install(ctx, in.GameDir, report, nil)
	}); errors.Is(err, job.ErrDuplicate) {
		fail(c, http.StatusConflict, err.Error())
		return
	} else if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	d.audit(c, in.ID, "instance.install", "安装/校验服务端")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (d Deps) handleInstanceUpdateCheck(c *gin.Context) {
	if d.Installer == nil {
		fail(c, http.StatusInternalServerError, "安装服务未配置")
		return
	}
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	local, remote, err := d.Installer.UpdateCheck(c.Request.Context(), in.GameDir, nil)
	if err != nil {
		fail(c, http.StatusBadGateway, "更新检查失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"local": local, "remote": remote, "up_to_date": local == remote})
}

// runUpdateJob 更新任务主体：优雅停服 → 更新前自动备份（pre-update，失败则
// 中止更新）→ SteamCMD 更新；更新后保持停止。handleInstanceUpdate 与调度器
// update 分派（M3-T9）共用；备份服务未装配时跳过备份步骤。
func (d Deps) runUpdateJob(ctx context.Context, inst instance.Instance, report func(int, string)) error {
	st, found := d.Sup.Status(inst.ID)
	if found && (st.State == supervisor.StateRunning || st.State == supervisor.StateStarting) {
		report(5, "优雅停服中")
		if err := d.Sup.Stop(inst.ID); err != nil && !errors.Is(err, supervisor.ErrNotRunning) {
			return fmt.Errorf("停服失败: %w", err)
		}
	} else if inst.Status == "running" {
		// 面板外残留进程：守护器无托管记录（如面板重启后失联），Sup.Stop 无法代走
		// 停机链，直接执行优雅停机钩子（REST Shutdown → RCON Exit，尽力而为）。
		report(5, "优雅停服中")
		d.stopOrphan(inst)
	}
	if d.BackupSvc != nil {
		report(8, "更新前自动备份")
		if _, err := d.BackupSvc.RunBackup(ctx, inst, "pre-update", "更新前自动备份"); err != nil {
			// 存档目录尚不存在（从未开服）视为无需保护，继续更新
			if !errors.Is(err, backup.ErrSavesMissing) {
				return fmt.Errorf("更新前备份失败: %w", err)
			}
		}
	}
	report(10, "开始下载/校验更新")
	return d.Installer.Install(ctx, inst.GameDir, report, nil)
}

// handleInstanceUpdate：运行中则先优雅停服 → pre-update 备份 → SteamCMD 更新 →
// 更新后保持停止（不自动拉起，由管理员确认后手动 start）。
func (d Deps) handleInstanceUpdate(c *gin.Context) {
	if d.Installer == nil || d.Jobs == nil {
		fail(c, http.StatusInternalServerError, "安装服务未配置")
		return
	}
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	if _, err := d.Jobs.Start("update", in.ID, func(ctx context.Context, report func(int, string)) error {
		return d.runUpdateJob(ctx, in, report)
	}); errors.Is(err, job.ErrDuplicate) {
		fail(c, http.StatusConflict, err.Error())
		return
	} else if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	d.audit(c, in.ID, "instance.update", "更新服务端")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// stopOrphan 对守护器外的残留进程执行优雅停机钩子（REST → RCON，尽力而为，
// 失败不阻断更新）；经优雅手段停下时同步 DB 状态为 idle，避免状态长期失真。
func (d Deps) stopOrphan(inst instance.Instance) {
	h := d.stopHooks(inst)
	stopped := false
	if h.REST != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		stopped = h.REST(ctx) == nil
		cancel()
	}
	if !stopped && h.RCON != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		stopped = h.RCON(ctx) == nil
		cancel()
	}
	if stopped {
		_, _ = d.DB.Exec(`UPDATE instances SET status='idle' WHERE id=?`, inst.ID)
	}
}

// ---- 状态/日志 ----

func (d Deps) handleInstanceStatus(c *gin.Context) {
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	out := gin.H{"status": in.Status, "metrics": nil}
	if st, found := d.Sup.Status(in.ID); found {
		out["status"] = st.State
		out["pid"], out["started_at"], out["restarts"] = st.PID, st.StartedAt, st.Restarts
		// metrics 尽力而为：REST 失败保持 null 不报错
		if client, err := d.restClient(in); err == nil {
			if m, err := client.Metrics(c.Request.Context()); err == nil {
				out["metrics"] = m
			}
		}
	}
	c.JSON(http.StatusOK, out)
}

// handleInstanceLogs 读 console.log 末 N 行（默认 200，上限 1000）；
// 文件缺失/读取失败按空日志返回（尽力而为语义）。
func (d Deps) handleInstanceLogs(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	tail := 200
	if v := c.Query("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			tail = n
		}
	}
	if tail > 1000 {
		tail = 1000
	}
	lines := []string{}
	f, err := os.Open(filepath.Join(d.Cfg.DataDir, "instances",
		strconv.FormatInt(id, 10), "logs", "console.log"))
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		if len(lines) > tail {
			lines = lines[len(lines)-tail:]
		}
	}
	c.JSON(http.StatusOK, gin.H{"lines": lines})
}

// ---- 玩家管理（官方 REST 网关） ----

func (d Deps) handleInstancePlayers(c *gin.Context) {
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	client, err := d.restClient(in)
	if err != nil {
		fail(c, http.StatusConflict, err.Error())
		return
	}
	ps, err := client.Players(c.Request.Context())
	if errors.Is(err, gateway.ErrAuth) {
		fail(c, http.StatusBadGateway, "REST 认证失败")
		return
	}
	if err != nil {
		fail(c, http.StatusBadGateway, "无法连接游戏服 REST API")
		return
	}
	c.JSON(http.StatusOK, gin.H{"players": ps})
}

// playerAction 统一 kick/ban：{"uid":"1"} → 网关 userid。
func (d Deps) playerAction(c *gin.Context, action string) {
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	var req struct {
		UID string `json:"uid"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.UID == "" {
		fail(c, http.StatusBadRequest, "参数不合法：uid 必填")
		return
	}
	client, err := d.restClient(in)
	if err != nil {
		fail(c, http.StatusConflict, err.Error())
		return
	}
	var callErr error
	switch action {
	case "kick":
		callErr = client.Kick(c.Request.Context(), req.UID)
	case "ban":
		callErr = client.Ban(c.Request.Context(), req.UID)
	}
	if errors.Is(callErr, gateway.ErrAuth) {
		fail(c, http.StatusBadGateway, "REST 认证失败")
		return
	}
	if callErr != nil {
		fail(c, http.StatusBadGateway, "操作失败: "+callErr.Error())
		return
	}
	d.audit(c, in.ID, "player."+action, "对玩家 "+req.UID+" 执行 "+action)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (d Deps) handlePlayerKick(c *gin.Context) { d.playerAction(c, "kick") }

func (d Deps) handlePlayerBan(c *gin.Context) { d.playerAction(c, "ban") }

func (d Deps) handlePlayerAnnounce(c *gin.Context) {
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Message == "" {
		fail(c, http.StatusBadRequest, "参数不合法：message 必填")
		return
	}
	client, err := d.restClient(in)
	if err != nil {
		fail(c, http.StatusConflict, err.Error())
		return
	}
	if err := client.Announce(c.Request.Context(), req.Message); err != nil {
		if errors.Is(err, gateway.ErrAuth) {
			fail(c, http.StatusBadGateway, "REST 认证失败")
			return
		}
		fail(c, http.StatusBadGateway, "广播失败: "+err.Error())
		return
	}
	d.audit(c, in.ID, "player.announce", "广播消息")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (d Deps) handlePlayerSave(c *gin.Context) {
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	client, err := d.restClient(in)
	if err != nil {
		fail(c, http.StatusConflict, err.Error())
		return
	}
	if err := client.Save(c.Request.Context()); err != nil {
		if errors.Is(err, gateway.ErrAuth) {
			fail(c, http.StatusBadGateway, "REST 认证失败")
			return
		}
		fail(c, http.StatusBadGateway, "保存失败: "+err.Error())
		return
	}
	d.audit(c, in.ID, "player.save", "手动保存世界")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
