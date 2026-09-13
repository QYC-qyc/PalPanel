-- backups 表补 sha256 列：备份落盘时引擎已计算整体摘要，落账一并存储，
-- 列表接口直接返回（避免列表时重算大文件哈希）。历史行留空串。
ALTER TABLE backups ADD COLUMN sha256 TEXT NOT NULL DEFAULT '';
