// Package instance 提供实例的持久化存储（含端口冲突检测）。
package instance

import (
	"database/sql"
	"errors"
)

var (
	ErrPortConflict = errors.New("端口与现有实例冲突")
	ErrNotFound     = errors.New("实例不存在")
)

type Instance struct {
	ID               int64
	Name             string
	GameDir          string
	BackupDir        string
	Status           string // 进程守护（M2）维护，store 的 Update 不修改
	GamePort         int
	QueryPort        int
	RestPort         int // 0 表示与 game_port 相同
	AdminPasswordEnc []byte
	RestEnabled      bool
	RconEnabled      bool
	Autostart        bool
}

type Store struct{ DB *sql.DB }

func New(d *sql.DB) *Store { return &Store{DB: d} }

func (s *Store) portConflict(in Instance) (bool, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM instances
		WHERE id<>? AND (game_port=? OR query_port=?)`, in.ID, in.GamePort, in.QueryPort).Scan(&n)
	return n > 0, err
}

func (s *Store) Create(in Instance) (int64, error) {
	bad, err := s.portConflict(in)
	if err != nil {
		return 0, err
	}
	if bad {
		return 0, ErrPortConflict
	}
	res, err := s.DB.Exec(`INSERT INTO instances
		(name, game_dir, backup_dir, game_port, query_port, rest_port, admin_password_enc, rest_enabled, rcon_enabled, autostart)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		in.Name, in.GameDir, in.BackupDir, in.GamePort, in.QueryPort, in.RestPort,
		in.AdminPasswordEnc, b2i(in.RestEnabled), b2i(in.RconEnabled), b2i(in.Autostart))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanInstance(row interface{ Scan(...any) error }) (Instance, error) {
	var in Instance
	var rest, rcon, auto int
	err := row.Scan(&in.ID, &in.Name, &in.GameDir, &in.BackupDir, &in.Status,
		&in.GamePort, &in.QueryPort, &in.RestPort, &in.AdminPasswordEnc, &rest, &rcon, &auto)
	in.RestEnabled, in.RconEnabled, in.Autostart = rest == 1, rcon == 1, auto == 1
	if errors.Is(err, sql.ErrNoRows) {
		return in, ErrNotFound
	}
	return in, err
}

const selectCols = `id, name, game_dir, backup_dir, status, game_port, query_port, rest_port,
	admin_password_enc, rest_enabled, rcon_enabled, autostart`

func (s *Store) Get(id int64) (Instance, error) {
	return scanInstance(s.DB.QueryRow(`SELECT `+selectCols+` FROM instances WHERE id=?`, id))
}

func (s *Store) List() ([]Instance, error) {
	rows, err := s.DB.Query(`SELECT ` + selectCols + ` FROM instances ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Instance
	for rows.Next() {
		// scanInstance 只 Scan 行数据、不发起内层查询，
		// MaxOpenConns(1) 下在 rows 迭代中再查询会死锁。
		in, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// Update 不修改 status 与 game_dir（game_dir 建后不可改，status 由进程守护维护）。
func (s *Store) Update(in Instance) error {
	bad, err := s.portConflict(in)
	if err != nil {
		return err
	}
	if bad {
		return ErrPortConflict
	}
	res, err := s.DB.Exec(`UPDATE instances SET name=?, backup_dir=?, game_port=?, query_port=?, rest_port=?,
		admin_password_enc=?, rest_enabled=?, rcon_enabled=?, autostart=? WHERE id=?`,
		in.Name, in.BackupDir, in.GamePort, in.QueryPort, in.RestPort, in.AdminPasswordEnc,
		b2i(in.RestEnabled), b2i(in.RconEnabled), b2i(in.Autostart), in.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Delete(id int64) error {
	res, err := s.DB.Exec(`DELETE FROM instances WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
