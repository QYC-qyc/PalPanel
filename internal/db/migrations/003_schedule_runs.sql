-- schedules 定时任务表（T3 发现 spec 中该表 001 未建，本迁移补建；
-- 列定义与 spec 2026-09-13-palworld-panel-design.md 一致）
CREATE TABLE schedules(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  instance_id INTEGER NOT NULL,
  kind TEXT NOT NULL,
  cron_expr TEXT NOT NULL,
  payload TEXT NOT NULL DEFAULT '{}',
  enabled INTEGER NOT NULL DEFAULT 1,
  last_run_at TEXT,
  next_run_at TEXT
);
CREATE INDEX idx_schedules_instance ON schedules(instance_id);
