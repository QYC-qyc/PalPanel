package backup

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"palpanel/internal/db"
)

// newStore 打开临时库并应用迁移（含 002 backups 表）。
func newStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(t.TempDir()) // db.Open 收数据目录，库文件固定为目录下 panel.db
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	return NewStore(d)
}

// setTime 直接改写某行的 created_at，绕过 datetime('now') 的秒级粒度，
// 以确定性地构造新旧次序。
func setTime(t *testing.T, s *Store, id int64, ts string) {
	t.Helper()
	if _, err := s.DB.Exec(`UPDATE backups SET created_at=? WHERE id=?`, ts, id); err != nil {
		t.Fatalf("set created_at: %v", err)
	}
}

func rec(instance int64, file string) BackupRecord {
	return BackupRecord{InstanceID: instance, File: file, SizeBytes: 1024,
		Type: "manual", Note: "备注-" + file}
}

// 组 1：Add 插入并返回自增 ID；List 按 created_at DESC 排序且字段完整读回
func TestAddAndListOrder(t *testing.T) {
	s := newStore(t)
	idA, err := s.Add(rec(1, "a.tar.gz"))
	if err != nil {
		t.Fatalf("Add a: %v", err)
	}
	idB, err := s.Add(BackupRecord{InstanceID: 1, File: "b.tar.gz", SizeBytes: 2048,
		Type: "pre-update", Note: "更新前"})
	if err != nil {
		t.Fatalf("Add b: %v", err)
	}
	if idA == idB {
		t.Fatalf("Add 应返回不同自增 ID，均为 %d", idA)
	}

	// 强制 a 的 created_at 早于 b（默认值同为秒级时间戳，需显式拉开）
	setTime(t, s, idA, "2020-01-01 00:00:00")

	got, err := s.List(1)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// b（新）在前，a（旧）在后
	if got[0].ID != idB || got[1].ID != idA {
		t.Errorf("order = [%d, %d], want [%d, %d] (created_at DESC)",
			got[0].ID, got[1].ID, idB, idA)
	}
	b := got[0]
	if b.InstanceID != 1 || b.File != "b.tar.gz" || b.SizeBytes != 2048 ||
		b.Type != "pre-update" || b.Note != "更新前" {
		t.Errorf("record = %+v", b)
	}
	// created_at 由表默认 datetime('now') 生成，读回应解析为非零 time.Time
	if b.CreatedAt.IsZero() {
		t.Errorf("CreatedAt 零值，应从文本 %q 解析", b.CreatedAt)
	}
	if _, err := time.Parse("2006-01-02 15:04:05", b.CreatedAt.Format("2006-01-02 15:04:05")); err != nil {
		t.Errorf("CreatedAt 格式异常: %v", err)
	}

	// 其他实例看不到实例 1 的记录
	if got2, err := s.List(2); err != nil || len(got2) != 0 {
		t.Errorf("List(2) = %v, %v; want 空", got2, err)
	}
}

// created_at 非法文本 → 置零值不报错（裁决：解析失败不视为错误）
func TestListBadTimestampZeroValue(t *testing.T) {
	s := newStore(t)
	if _, err := s.DB.Exec(`INSERT INTO backups(instance_id, file, size, type, note, created_at)
		VALUES(1, 'x.tar.gz', 1, 'manual', '', 'not-a-date')`); err != nil {
		t.Fatal(err)
	}
	got, err := s.List(1)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || !got[0].CreatedAt.IsZero() {
		t.Errorf("CreatedAt = %v, want 零值且不报错", got[0].CreatedAt)
	}
}

// 组 2：Get 按 (instanceID, backupID) 命中；跨实例/不存在 → ErrNotFound
func TestGetCrossInstanceIsolation(t *testing.T) {
	s := newStore(t)
	id, err := s.Add(rec(1, "x.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(1, id)
	if err != nil {
		t.Fatalf("Get(1,%d): %v", id, err)
	}
	if got.File != "x.tar.gz" {
		t.Errorf("File = %q, want x.tar.gz", got.File)
	}
	if _, err := s.Get(2, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("跨实例 Get err = %v, want ErrNotFound", err)
	}
	if _, err := s.Get(1, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在 Get err = %v, want ErrNotFound", err)
	}
}

// 组 3：Delete 返回库中记录的文件路径并删行；跨实例/重复删除 → ErrNotFound
func TestDeleteReturnsFile(t *testing.T) {
	s := newStore(t)
	path := filepath.Join(t.TempDir(), "backups", "del.tar.gz")
	id, err := s.Add(BackupRecord{InstanceID: 1, File: path, SizeBytes: 8, Type: "scheduled"})
	if err != nil {
		t.Fatal(err)
	}
	// 跨实例删除必须拒绝且不删行
	if _, err := s.Delete(2, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("跨实例 Delete err = %v, want ErrNotFound", err)
	}
	if n, err := s.Count(1); err != nil || n != 1 {
		t.Fatalf("跨实例误删: Count(1) = %d, %v", n, err)
	}

	file, err := s.Delete(1, id)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if file != path {
		t.Errorf("file = %q, want %q（应返回库中记录值，文件由调用方删）", file, path)
	}
	if n, err := s.Count(1); err != nil || n != 0 {
		t.Errorf("Count(1) = %d, %v; want 0", n, err)
	}
	if _, err := s.Delete(1, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复 Delete err = %v, want ErrNotFound", err)
	}
}

// 组 4：CleanupOldest 保留 created_at 最新的 keepCount 条，只删库行；
// 不波及其他实例；keepCount >= 现有条数时删 0
func TestCleanupOldestKeepsNewest(t *testing.T) {
	s := newStore(t)
	var ids [5]int64
	for i := 0; i < 5; i++ {
		id, err := s.Add(rec(1, filepath.Join("bk", string(rune('1'+i))+".tar.gz")))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		setTime(t, s, id, "2026-01-0"+string(rune('1'+i))+" 00:00:00") // 依次变新
	}
	idOther, err := s.Add(rec(2, "other.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := s.CleanupOldest(1, 2)
	if err != nil {
		t.Fatalf("CleanupOldest: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3", deleted)
	}
	got, err := s.List(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("剩余 %d 条, want 2", len(got))
	}
	// 保留的应是最新的 ids[4]、ids[3]
	if got[0].ID != ids[4] || got[1].ID != ids[3] {
		t.Errorf("kept = [%d, %d], want [%d, %d]", got[0].ID, got[1].ID, ids[4], ids[3])
	}
	// 其他实例不受影响
	if n, err := s.Count(2); err != nil || n != 1 {
		t.Errorf("Count(2) = %d, %v; want 1", n, err)
	}
	if _, err := s.Get(2, idOther); err != nil {
		t.Errorf("其他实例记录被误删: %v", err)
	}

	// keepCount >= 条数 → 删 0
	if deleted, err := s.CleanupOldest(1, 5); err != nil || deleted != 0 {
		t.Errorf("CleanupOldest(1,5) = %d, %v; want 0", deleted, err)
	}
	// 防御性：keepCount<=0 视为不清理（避免误删全部），文档注释已写明
	if deleted, err := s.CleanupOldest(1, 0); err != nil || deleted != 0 {
		t.Errorf("CleanupOldest(1,0) = %d, %v; want 0", deleted, err)
	}
	if n, _ := s.Count(1); n != 2 {
		t.Errorf("keepCount=0 后剩余 %d 条, want 2", n)
	}
}

// Count 按实例计数
func TestCount(t *testing.T) {
	s := newStore(t)
	if n, err := s.Count(7); err != nil || n != 0 {
		t.Fatalf("空表 Count = %d, %v; want 0", n, err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Add(rec(7, "f.tar.gz")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Add(rec(8, "g.tar.gz")); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Count(7); err != nil || n != 3 {
		t.Errorf("Count(7) = %d, %v; want 3", n, err)
	}
}
