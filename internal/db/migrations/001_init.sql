CREATE TABLE users(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE COLLATE NOCASE,
  display_name TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL,
  is_active INTEGER NOT NULL DEFAULT 1,
  role_version INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE roles(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  description TEXT NOT NULL DEFAULT '',
  is_builtin INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE permissions(code TEXT PRIMARY KEY);
CREATE TABLE role_permissions(
  role_id INTEGER NOT NULL, code TEXT NOT NULL,
  PRIMARY KEY(role_id, code)
);
CREATE TABLE user_roles(
  user_id INTEGER NOT NULL, role_id INTEGER NOT NULL,
  PRIMARY KEY(user_id, role_id)
);
CREATE TABLE instance_grants(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER, role_id INTEGER, instance_id INTEGER NOT NULL,
  CHECK((user_id IS NULL) <> (role_id IS NULL)),
  UNIQUE(user_id, instance_id), UNIQUE(role_id, instance_id)
);
CREATE TABLE instances(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  game_dir TEXT NOT NULL UNIQUE,
  backup_dir TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'idle',
  game_port INTEGER NOT NULL DEFAULT 8211,
  query_port INTEGER NOT NULL DEFAULT 27015,
  rest_port INTEGER NOT NULL DEFAULT 0,
  admin_password_enc BLOB NOT NULL,
  rest_enabled INTEGER NOT NULL DEFAULT 1,
  rcon_enabled INTEGER NOT NULL DEFAULT 1,
  autostart INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE audit_logs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER, username TEXT NOT NULL DEFAULT '',
  instance_id INTEGER, action TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_audit_created ON audit_logs(created_at DESC);
