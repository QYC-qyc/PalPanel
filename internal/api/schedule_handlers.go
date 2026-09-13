// 定时任务端点：schedules CRUD（schedule.manage 权限，实例级）。
// 写操作落库后调用 Sched.Reload() 热加载；cron 表达式落库前经 cronexpr.Parse
// 校验（非法 400），kind 白名单 backup/restart/broadcast/update。
// 新建行预置 last_run_at=now（规避调度器 Reload 对新行的立即补跑），
// next_run_at 交给调度器/Reload 计算，API 不算。
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"palpanel/internal/auth"
	"palpanel/internal/cronexpr"
	"palpanel/internal/instance"
	"palpanel/internal/job"
	"palpanel/internal/scheduler"
	"palpanel/internal/supervisor"
)

// ScheduleReloader 是调度器热加载的最小接口（生产 *scheduler.Scheduler，测试注入 fake）。
type ScheduleReloader interface {
	Reload()
}

func (d Deps) registerSchedules(v1 *gin.RouterGroup) {
	g := v1.Group("/instances", d.authRequired())
	g.GET("/:id/schedules", d.requirePerm(auth.PScheduleManage), d.handleScheduleList)
	g.POST("/:id/schedules", d.requirePerm(auth.PScheduleManage), d.handleScheduleCreate)
	g.PATCH("/:id/schedules/:sid", d.requirePerm(auth.PScheduleManage), d.handleSchedulePatch)
	g.DELETE("/:id/schedules/:sid", d.requirePerm(auth.PScheduleManage), d.handleScheduleDelete)
}

// scheduleKinds 是 kind 白名单（与调度器分派的四类执行体一致）。
var scheduleKinds = map[string]bool{"backup": true, "restart": true, "broadcast": true, "update": true}

type scheduleDTO struct {
	ID         int64           `json:"id"`
	Kind       string          `json:"kind"`
	CronExpr   string          `json:"cron_expr"`
	Payload    json.RawMessage `json:"payload"`
	Enabled    bool            `json:"enabled"`
	LastRunAt  *string         `json:"last_run_at"`
	NextRunAt  *string         `json:"next_run_at"`
}

// scanSchedule 行扫描：payload 原样回传（写入端已保证是合法 JSON 文本）。
func scanSchedule(row interface{ Scan(...any) error }) (scheduleDTO, error) {
	var s scheduleDTO
	var enabled int64
	var payload string
	var last, next sql.NullString
	if err := row.Scan(&s.ID, &s.Kind, &s.CronExpr, &payload, &enabled, &last, &next); err != nil {
		return s, err
	}
	s.Payload = json.RawMessage(payload)
	s.Enabled = enabled != 0
	if last.Valid {
		s.LastRunAt = &last.String
	}
	if next.Valid {
		s.NextRunAt = &next.String
	}
	return s, nil
}

const scheduleQueryCols = `id, kind, cron_expr, payload, enabled, last_run_at, next_run_at`

func (d Deps) handleScheduleList(c *gin.Context) {
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	rows, err := d.DB.Query(`SELECT `+scheduleQueryCols+` FROM schedules
		WHERE instance_id=? ORDER BY id`, in.ID)
	if err != nil {
		fail(c, http.StatusInternalServerError, "查询失败")
		return
	}
	defer rows.Close()
	out := []scheduleDTO{}
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			fail(c, http.StatusInternalServerError, "查询失败")
			return
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		fail(c, http.StatusInternalServerError, "查询失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"schedules": out})
}

type scheduleReq struct {
	Kind     *string          `json:"kind"`
	CronExpr *string          `json:"cron_expr"`
	Payload  *json.RawMessage `json:"payload"`
	Enabled  *bool            `json:"enabled"`
}

// validateSchedule 校验 kind 白名单与 cron 表达式，返回错误信息（空串为通过）。
func validateSchedule(kind, cron string) string {
	if !scheduleKinds[kind] {
		return "参数不合法：kind 须为 backup/restart/broadcast/update"
	}
	if _, err := cronexpr.Parse(cron); err != nil {
		return "参数不合法：cron_expr 无效"
	}
	return ""
}

// reloadSched 写操作成功后的热加载（调度器未装配时静默跳过）。
func (d Deps) reloadSched() {
	if d.Sched != nil {
		d.Sched.Reload()
	}
}

func (d Deps) handleScheduleCreate(c *gin.Context) {
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	var req scheduleReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Kind == nil || req.CronExpr == nil {
		fail(c, http.StatusBadRequest, "参数不合法：kind/cron_expr 必填")
		return
	}
	if msg := validateSchedule(*req.Kind, *req.CronExpr); msg != "" {
		fail(c, http.StatusBadRequest, msg)
		return
	}
	payload := "{}"
	if req.Payload != nil {
		payload = string(*req.Payload)
	}
	// 预置 last_run_at=now（UTC 文本）：新行 Reload 时按「已执行过、next 缺失」处理，
	// 只修复 next_run_at 而不立即补跑（M3-T5 裁决注记）。
	lastRun := time.Now().UTC().Format("2006-01-02 15:04:05")
	res, err := d.DB.Exec(`INSERT INTO schedules(instance_id, kind, cron_expr, payload, enabled, last_run_at)
		VALUES(?,?,?,?,1,?)`, in.ID, *req.Kind, *req.CronExpr, payload, lastRun)
	if err != nil {
		fail(c, http.StatusInternalServerError, "创建失败")
		return
	}
	id, _ := res.LastInsertId()
	d.audit(c, in.ID, "schedule.manage", "创建定时任务 #"+strconv.FormatInt(id, 10)+" ("+*req.Kind+")")
	d.reloadSched()
	c.JSON(http.StatusOK, gin.H{"id": id})
}

func (d Deps) handleSchedulePatch(c *gin.Context) {
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	sid, ok2 := parseSubID(c, "sid")
	if !ok2 {
		return
	}
	cur, err := scanSchedule(d.DB.QueryRow(`SELECT `+scheduleQueryCols+`
		FROM schedules WHERE id=? AND instance_id=?`, sid, in.ID))
	if err == sql.ErrNoRows {
		fail(c, http.StatusNotFound, "定时任务不存在")
		return
	}
	if err != nil {
		fail(c, http.StatusInternalServerError, "查询失败")
		return
	}
	var req scheduleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数不合法")
		return
	}
	kind, cron := cur.Kind, cur.CronExpr
	if req.Kind != nil {
		kind = *req.Kind
	}
	if req.CronExpr != nil {
		cron = *req.CronExpr
	}
	if msg := validateSchedule(kind, cron); msg != "" {
		fail(c, http.StatusBadRequest, msg)
		return
	}
	payload := string(cur.Payload)
	if req.Payload != nil {
		payload = string(*req.Payload)
	}
	enabled := cur.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	// next_run_at 置空：kind/cron 变更后由 Reload 重锚（不立即补跑）。
	en := 0
	if enabled {
		en = 1
	}
	if _, err := d.DB.Exec(`UPDATE schedules SET kind=?, cron_expr=?, payload=?, enabled=?, next_run_at=NULL
		WHERE id=? AND instance_id=?`, kind, cron, payload, en, sid, in.ID); err != nil {
		fail(c, http.StatusInternalServerError, "保存失败")
		return
	}
	d.audit(c, in.ID, "schedule.manage", "修改定时任务 #"+strconv.FormatInt(sid, 10))
	d.reloadSched()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (d Deps) handleScheduleDelete(c *gin.Context) {
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	sid, ok2 := parseSubID(c, "sid")
	if !ok2 {
		return
	}
	res, err := d.DB.Exec(`DELETE FROM schedules WHERE id=? AND instance_id=?`, sid, in.ID)
	if err != nil {
		fail(c, http.StatusInternalServerError, "删除失败")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(c, http.StatusNotFound, "定时任务不存在")
		return
	}
	d.audit(c, in.ID, "schedule.manage", "删除定时任务 #"+strconv.FormatInt(sid, 10))
	d.reloadSched()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// RunScheduled 是调度器的执行体分派（main 装配为 scheduler.RunFunc 注入）：
// backup→定时备份（type=scheduled）；broadcast→REST Announce 优先、失败退 RCON；
// restart→停服（容忍未运行）后拉起；update→复用 runUpdateJob（含 restore 互斥，
// 同 kind 去重时视为跳过不算失败）。实例已删除时返回错误由调度器记日志。
func (d Deps) RunScheduled(row scheduler.SchedulesRow) error {
	inst, err := d.Instances.Get(row.InstanceID)
	if err != nil {
		return fmt.Errorf("实例 %d 不存在或查询失败: %w", row.InstanceID, err)
	}
	switch row.Kind {
	case "backup":
		// 恢复/更新会覆写存档，与其并发打包会打出半清空的备份包：跳过本周期
		if d.Jobs != nil && (d.Jobs.RunningOfKind(inst.ID, "restore") || d.Jobs.RunningOfKind(inst.ID, "update")) {
			return errors.New("恢复/更新任务进行中，跳过本次定时备份")
		}
		if d.BackupSvc == nil {
			return errors.New("备份服务未配置")
		}
		if d.Jobs == nil {
			return errors.New("任务服务未配置")
		}
		// 统一走 job 体系：与手动备份同 kind 去重（ErrDuplicate → 跳过本周期不算
		// 失败），且 RunningOfKind("backup") 能被 restore/update 的互斥检查感知。
		_, err := d.Jobs.Start("backup", inst.ID, func(ctx context.Context, report func(int, string)) error {
			_, err := d.BackupSvc.RunBackup(ctx, inst, "scheduled", "定时备份")
			return err
		})
		if errors.Is(err, job.ErrDuplicate) {
			return nil
		}
		return err
	case "broadcast":
		return d.runScheduledBroadcast(inst, row.Payload)
	case "restart":
		// 与 API 启动端点同源互斥：恢复/更新期间定时重启会把实例在存档被覆写时拉起
		if d.Jobs != nil && (d.Jobs.RunningOfKind(inst.ID, "restore") || d.Jobs.RunningOfKind(inst.ID, "update")) {
			return errors.New("恢复/更新任务进行中，跳过本次定时重启")
		}
		if d.Sup != nil {
			if err := d.Sup.Stop(inst.ID); err != nil && !errors.Is(err, supervisor.ErrNotRunning) {
				return fmt.Errorf("停服失败: %w", err)
			}
		}
		return d.StartInstance(inst.ID)
	case "update":
		if d.Installer == nil {
			return errors.New("安装服务未配置")
		}
		if d.Jobs == nil {
			return errors.New("任务服务未配置")
		}
		if _, err := d.Jobs.Start("update", inst.ID, func(ctx context.Context, report func(int, string)) error {
			return d.runUpdateJob(ctx, inst, report)
		}); errors.Is(err, job.ErrDuplicate) {
			return nil // 本周期已有更新在跑：跳过（防重语义，不算失败）
		} else {
			return err
		}
	default:
		return fmt.Errorf("未知任务类型 %q", row.Kind)
	}
}

// runScheduledBroadcast 定时广播：payload 形如 {"message":"..."}；
// REST 官方网关优先，失败退 RCON Broadcast（两者皆败返回错误由调度器记日志）。
func (d Deps) runScheduledBroadcast(inst instance.Instance, payload string) error {
	var p struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil || p.Message == "" {
		return errors.New("广播 payload 不合法：需要 {\"message\":\"...\"}")
	}
	ctx := context.Background()
	if client, err := d.restClient(inst); err == nil {
		if err := client.Announce(ctx, p.Message); err == nil {
			return nil
		}
	}
	if conn, err := d.rconClient(inst); err == nil {
		_, execErr := conn.Exec("Broadcast " + p.Message)
		_ = conn.Close()
		return execErr
	}
	return errors.New("REST 与 RCON 均不可用，广播未送达")
}
