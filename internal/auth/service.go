package auth

import (
	"database/sql"
	"errors"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrSetupDone    = errors.New("初始化已完成，不能重复设置管理员")
	ErrWeakPassword = errors.New("密码强度不足：至少 8 位")
	ErrBadCreds     = errors.New("用户名或密码错误")
	ErrInactive     = errors.New("账号已停用")
	ErrStaleToken   = errors.New("登录已过期，请重新登录")
)

type Service struct {
	DB     *sql.DB
	Secret []byte
}

func New(d *sql.DB, secret []byte) *Service { return &Service{DB: d, Secret: secret} }

func (s *Service) userCount() (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Service) Setup(username, password string) (int64, error) {
	if len(password) < 8 {
		return 0, ErrWeakPassword
	}
	if n, err := s.userCount(); err != nil {
		return 0, err
	} else if n > 0 {
		return 0, ErrSetupDone
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return 0, err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO users(username, password_hash) VALUES(?,?)`, username, string(hash))
	if err != nil {
		return 0, err
	}
	uid, _ := res.LastInsertId()
	var adminID int64
	if err := tx.QueryRow(`SELECT id FROM roles WHERE name='admin'`).Scan(&adminID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO user_roles(user_id, role_id) VALUES(?,?)`, uid, adminID); err != nil {
		return 0, err
	}
	return uid, tx.Commit()
}
