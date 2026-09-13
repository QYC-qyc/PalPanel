// Service 是备份引擎与元数据存储的薄封装：把「打包落盘 → 落账 → 滚动清理」
// 与「校验 → 恢复前自动备份 → 停服 → 恢复」两条业务链收拢为一处，
// 供 API 层（手动备份/恢复 job）、update job（pre-update 备份）与
// 调度器分派（Task 9）复用。
//
// 停服手段经 NewService 注入（生产传 supervisor.Stop 的包装：容忍未运行；
// 测试可注入 fake），本包不依赖 supervisor。
package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"palpanel/internal/instance"
)

// ErrSavesMissing 表示实例的存档目录不存在（备份应报错；恢复前自动备份可容忍）。
var ErrSavesMissing = errors.New("存档目录不存在")

// Service 组合备份引擎（包函数）+ Store + 停服手段 + 滚动保留条数。
type Service struct {
	store     *Store
	stopper   func(id int64) error // 停服（调用方保证对未运行实例返回 nil）
	keepCount int                  // 滚动清理保留条数（<=0 不清理）
}

// NewService 构造 Service。stopper 可为 nil（恢复时不经停服，仅测试场景）。
func NewService(store *Store, stopper func(id int64) error, keepCount int) *Service {
	return &Service{store: store, stopper: stopper, keepCount: keepCount}
}

// SavesDir 返回实例的存档目录（Pal 服务器约定路径）：<gameDir>/Pal/Saved/SaveGames/0。
func SavesDir(gameDir string) string {
	return filepath.Join(gameDir, "Pal", "Saved", "SaveGames", "0")
}

// BackupDirFor 返回备份文件存放目录：实例配置了 backup_dir 用之，
// 否则落 <gameDir>/backups（跟随实例目录，删实例即整体清理）。
func BackupDirFor(inst instance.Instance) string {
	if inst.BackupDir != "" {
		return inst.BackupDir
	}
	return filepath.Join(inst.GameDir, "backups")
}

// RunBackup 执行一次完整备份：存档目录守卫 → tar.gz 打包 → Store 落账 →
// 滚动清理（含被滚出记录的文件删除）。typ 取 manual/scheduled/pre-update/pre-restore。
func (s *Service) RunBackup(ctx context.Context, inst instance.Instance, typ, note string) (BackupRecord, error) {
	saves := SavesDir(inst.GameDir)
	if _, err := os.Stat(saves); err != nil {
		return BackupRecord{}, fmt.Errorf("%w: %s", ErrSavesMissing, saves)
	}
	dest := filepath.Join(BackupDirFor(inst),
		fmt.Sprintf("backup-%d-%s-%s.tar.gz", inst.ID, typ, time.Now().UTC().Format("20060102-150405")))
	info, err := Create(ctx, saves, dest, nil)
	if err != nil {
		return BackupRecord{}, err
	}
	id, err := s.store.Add(BackupRecord{
		InstanceID: inst.ID, File: dest, SizeBytes: info.SizeBytes,
		SHA256: info.SHA256, Type: typ, Note: note,
	})
	if err != nil {
		_ = os.Remove(dest) // 落账失败不留孤儿文件
		return BackupRecord{}, err
	}
	s.cleanup(inst.ID)
	return BackupRecord{ID: id, InstanceID: inst.ID, File: dest, SizeBytes: info.SizeBytes,
		SHA256: info.SHA256, Type: typ, Note: note}, nil
}

// RunRestore 安全恢复：先 Verify（防损坏包破坏现场）→ 恢复前自动备份当前存档
// （type=pre-restore；存档目录不存在视为无需保护，继续恢复）→ 运行中先停服
// （经注入的 stopper，完成后保持停止，由用户手动启动）→ Restore（引擎内建
// 清空目标目录 + 路径逃逸守卫）。
func (s *Service) RunRestore(ctx context.Context, inst instance.Instance, rec BackupRecord) error {
	if _, err := Verify(rec.File); err != nil {
		return fmt.Errorf("备份包校验失败: %w", err)
	}
	if _, err := s.RunBackup(ctx, inst, "pre-restore", "恢复前自动备份"); err != nil && !errors.Is(err, ErrSavesMissing) {
		return fmt.Errorf("恢复前备份失败: %w", err)
	}
	if s.stopper != nil {
		if err := s.stopper(inst.ID); err != nil {
			return fmt.Errorf("恢复前停服失败: %w", err)
		}
	}
	return Restore(ctx, rec.File, SavesDir(inst.GameDir), nil)
}

// cleanup 滚动清理：保留最新 keepCount 条；被滚出的记录文件一并删除。
// before 按 created_at DESC 排序，末尾 n 条即被清理的最旧记录。
func (s *Service) cleanup(instanceID int64) {
	if s.keepCount <= 0 {
		return
	}
	before, err := s.store.List(instanceID)
	if err != nil {
		return
	}
	n, err := s.store.CleanupOldest(int(instanceID), s.keepCount)
	if err != nil || n <= 0 || n > len(before) {
		return
	}
	for _, rec := range before[len(before)-n:] {
		_ = os.Remove(rec.File)
	}
}
