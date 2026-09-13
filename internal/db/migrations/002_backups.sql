-- backups 备份元数据表（plan 原假定 001 已建，实际缺失，此处补建；
-- 列定义与 spec 2026-09-13-palworld-panel-design.md 一致）
CREATE TABLE backups(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  instance_id INTEGER NOT NULL,
  file TEXT NOT NULL,
  size INTEGER NOT NULL DEFAULT 0,
  type TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_backups_instance ON backups(instance_id, created_at DESC);
