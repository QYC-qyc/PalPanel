// 备份端点：列表 / 手动备份（异步 job，热备份） / 安全恢复（异步 job） / 删除。
// 备份三来源共用 BackupSvc（manual / scheduled 由调度器分派（Task 9） / pre-update
// 由 update job 调用）；删除备份复用 backup.create 权限码（裁决），审计 action 记 backup.delete。
package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"palpanel/internal/auth"
	"palpanel/internal/backup"
	"palpanel/internal/event"
	"palpanel/internal/instance"
	"palpanel/internal/job"
)

// BackupRunner 是备份服务的最小接口（生产 *backup.Service，测试可注入 fake）。
type BackupRunner interface {
	RunBackup(ctx context.Context, inst instance.Instance, typ, note string) (backup.BackupRecord, error)
	RunRestore(ctx context.Context, inst instance.Instance, rec backup.BackupRecord) error
}

// parseSubID 解析实例子资源的路径 ID（:bid/:sid），避开实例 :id 语义。
// 失败时已写响应并返回 false。
func parseSubID(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		fail(c, http.StatusBadRequest, "ID 不合法")
		return 0, false
	}
	return id, true
}

func (d Deps) registerBackup(v1 *gin.RouterGroup) {
	// 备份子路由参数用 :bid，避开实例 :id 语义；:id 仍由 requirePerm 做实例级权限判定
	g := v1.Group("/instances", d.authRequired())
	g.GET("/:id/backup", d.requirePerm(auth.PBackupRead), d.handleBackupList)
	g.POST("/:id/backup", d.requirePerm(auth.PBackupCreate), d.handleBackupCreate)
	g.POST("/:id/backup/:bid/restore", d.requirePerm(auth.PBackupRestore), d.handleBackupRestore)
	g.DELETE("/:id/backup/:bid", d.requirePerm(auth.PBackupCreate), d.handleBackupDelete)
}

type backupDTO struct {
	ID        int64     `json:"id"`
	File      string    `json:"file"` // 文件名（不含路径）
	SizeBytes int64     `json:"size_bytes"`
	SHA256    string    `json:"sha256"`
	Type      string    `json:"type"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

func (d Deps) handleBackupList(c *gin.Context) {
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	records, err := d.Backups.List(in.ID)
	if err != nil {
		fail(c, http.StatusInternalServerError, "查询失败")
		return
	}
	out := []backupDTO{}
	for _, rec := range records {
		out = append(out, backupDTO{ID: rec.ID, File: filepath.Base(rec.File),
			SizeBytes: rec.SizeBytes, SHA256: rec.SHA256, Type: rec.Type,
			Note: rec.Note, CreatedAt: rec.CreatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"backups": out})
}

// handleBackupCreate：手动备份走异步 job kind="backup"（裁决：热备份允许，
// 打包本身只读）；完成时由 BackupSvc 插入 type=manual 记录并滚动清理。
func (d Deps) handleBackupCreate(c *gin.Context) {
	if d.BackupSvc == nil || d.Jobs == nil {
		fail(c, http.StatusInternalServerError, "备份服务未配置")
		return
	}
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数不合法")
		return
	}
	jobID, err := d.Jobs.Start("backup", in.ID, func(ctx context.Context, report func(int, string)) error {
		report(10, "打包存档中")
		_, err := d.BackupSvc.RunBackup(ctx, in, "manual", req.Note)
		return err
	})
	if errors.Is(err, job.ErrDuplicate) {
		fail(c, http.StatusConflict, err.Error())
		return
	} else if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	d.audit(c, in.ID, "backup.create", "手动备份")
	c.JSON(http.StatusOK, gin.H{"ok": true, "job_id": jobID})
}

// handleBackupRestore：安全恢复走异步 job kind="restore"，流程见 backup.Service.RunRestore；
// 完成后实例保持停止并广播事件（用户手动启动）。
func (d Deps) handleBackupRestore(c *gin.Context) {
	if d.BackupSvc == nil || d.Jobs == nil {
		fail(c, http.StatusInternalServerError, "备份服务未配置")
		return
	}
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	bid, ok2 := parseSubID(c, "bid")
	if !ok2 {
		return
	}
	rec, err := d.Backups.Get(in.ID, bid)
	if errors.Is(err, backup.ErrNotFound) {
		fail(c, http.StatusNotFound, "备份不存在")
		return
	}
	if err != nil {
		fail(c, http.StatusInternalServerError, "查询失败")
		return
	}
	jobID, err := d.Jobs.Start("restore", in.ID, func(ctx context.Context, report func(int, string)) error {
		report(5, "校验并恢复中")
		if err := d.BackupSvc.RunRestore(ctx, in, rec); err != nil {
			return err
		}
		d.Hub.Broadcast(event.Event{Type: "backup.restored", InstanceID: in.ID,
			Payload: gin.H{"backup_id": rec.ID, "file": filepath.Base(rec.File)}})
		return nil
	})
	if errors.Is(err, job.ErrDuplicate) {
		fail(c, http.StatusConflict, err.Error())
		return
	} else if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	d.audit(c, in.ID, "backup.restore", "恢复备份 #"+strconv.FormatInt(bid, 10))
	c.JSON(http.StatusOK, gin.H{"ok": true, "job_id": jobID})
}

func (d Deps) handleBackupDelete(c *gin.Context) {
	if d.Backups == nil {
		fail(c, http.StatusInternalServerError, "备份服务未配置")
		return
	}
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	bid, ok2 := parseSubID(c, "bid")
	if !ok2 {
		return
	}
	file, err := d.Backups.Delete(in.ID, bid)
	if errors.Is(err, backup.ErrNotFound) {
		fail(c, http.StatusNotFound, "备份不存在")
		return
	}
	if err != nil {
		fail(c, http.StatusInternalServerError, "删除失败")
		return
	}
	if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
		fail(c, http.StatusInternalServerError, "删除备份文件失败")
		return
	}
	d.audit(c, in.ID, "backup.delete", "删除备份 #"+strconv.FormatInt(bid, 10))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
