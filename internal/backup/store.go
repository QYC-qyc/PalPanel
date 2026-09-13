package backup

import (
	"database/sql"
	"errors"
	"time"
)

// ErrNotFound 表示备份记录不存在（或 instanceID 与 backupID 不匹配——跨实例隔离）。
var ErrNotFound = errors.New("备份记录不存在")

// BackupRecord 是 backups 表的一行备份元数据。
// Type 取值：manual / scheduled / pre-update / pre-restore。
type BackupRecord struct {
	ID         int64
	InstanceID int64
	File       string // 备份文件路径（库中记录值）
	SizeBytes  int64  // 对应表列 size
	SHA256     string // 备份包整体摘要（落账时由引擎计算；历史行为空）
	Type       string
	Note       string
	CreatedAt  time.Time
}

// Store 是备份元数据的 SQLite 存取层。只管库行；
// 备份文件本身的落盘/删除由调用方负责。
type Store struct{ DB *sql.DB }

// NewStore 基于已打开并完成迁移的 *sql.DB 构造 Store。
func NewStore(db *sql.DB) *Store { return &Store{DB: db} }

// Add 插入一条备份记录并返回自增 ID。
// created_at 不传入，由表默认 datetime('now') 生成。
func (s *Store) Add(b BackupRecord) (int64, error) {
	res, err := s.DB.Exec(`INSERT INTO backups(instance_id, file, size, sha256, type, note)
		VALUES(?,?,?,?,?,?)`, b.InstanceID, b.File, b.SizeBytes, b.SHA256, b.Type, b.Note)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// parseTimestamp 把 SQLite 的 datetime 文本转为 time.Time。
// 解析失败置零值不报错（裁决：时间列仅用于展示/排序，容忍脏数据）。
func parseTimestamp(ts string) time.Time {
	if t, err := time.Parse("2006-01-02 15:04:05", ts); err == nil {
		return t
	}
	return time.Time{}
}

func scanBackup(row interface{ Scan(...any) error }) (BackupRecord, error) {
	var b BackupRecord
	var ts string
	err := row.Scan(&b.ID, &b.InstanceID, &b.File, &b.SizeBytes, &b.SHA256, &b.Type, &b.Note, &ts)
	b.CreatedAt = parseTimestamp(ts)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}

const backupCols = `id, instance_id, file, size, sha256, type, note, created_at`

// List 返回某实例的全部备份记录，created_at DESC（新→旧；
// 同秒以 id DESC 决胜，保证同批插入时次序稳定）。
func (s *Store) List(instanceID int64) ([]BackupRecord, error) {
	rows, err := s.DB.Query(`SELECT `+backupCols+` FROM backups
		WHERE instance_id=? ORDER BY created_at DESC, id DESC`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackupRecord
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Get 按 (instanceID, backupID) 精确读取；instanceID 不匹配视为不存在
// （跨实例隔离），返回 ErrNotFound。
func (s *Store) Get(instanceID, backupID int64) (BackupRecord, error) {
	return scanBackup(s.DB.QueryRow(`SELECT `+backupCols+` FROM backups
		WHERE id=? AND instance_id=?`, backupID, instanceID))
}

// Delete 删除某实例下的一条备份记录，并返回库中记录的文件路径，
// 供调用方删除磁盘文件（本方法只删库行，不触碰文件）。
// instanceID 不匹配或记录不存在 → ErrNotFound。
func (s *Store) Delete(instanceID, backupID int64) (string, error) {
	var file string
	err := s.DB.QueryRow(`SELECT file FROM backups
		WHERE id=? AND instance_id=?`, backupID, instanceID).Scan(&file)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	res, err := s.DB.Exec(`DELETE FROM backups WHERE id=? AND instance_id=?`, backupID, instanceID)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	return file, nil
}

// Count 返回某实例的备份记录条数。
func (s *Store) Count(instanceID int64) (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM backups WHERE instance_id=?`, instanceID).Scan(&n)
	return n, err
}

// CleanupOldest 滚动清理：保留 created_at 最新的 keepCount 条（同秒以 id 新者优先），
// 删除该实例其余的库行并返回删除条数——只删库行，对应备份文件由调用方删除。
// 防御性约定：keepCount<=0 视为不清理（返回 0），避免调用方配置异常时误删全部备份。
func (s *Store) CleanupOldest(instanceID, keepCount int) (int, error) {
	if keepCount <= 0 {
		return 0, nil
	}
	res, err := s.DB.Exec(`DELETE FROM backups WHERE instance_id=? AND id NOT IN (
		SELECT id FROM backups WHERE instance_id=?
		ORDER BY created_at DESC, id DESC LIMIT ?)`, instanceID, instanceID, keepCount)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
