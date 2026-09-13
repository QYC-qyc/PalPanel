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

type SessionUser struct {
	ID          int64    `json:"id"`
	Username    string   `json:"username"`
	DisplayName string   `json:"display_name"`
	IsActive    bool     `json:"is_active"`
	RoleVersion int64    `json:"-"`
	Roles       []string `json:"roles,omitempty"`
}

func (s *Service) Login(username, password string) (string, error) {
	var (
		id     int64
		rv     int64
		hash   string
		active bool
	)
	err := s.DB.QueryRow(`SELECT id, role_version, password_hash, is_active FROM users WHERE username=?`,
		username).Scan(&id, &rv, &hash, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrBadCreds
	}
	if err != nil {
		return "", err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return "", ErrBadCreds
	}
	if !active {
		return "", ErrInactive
	}
	return SignToken(s.Secret, id, rv)
}

func (s *Service) SessionUser(userID, rv int64) (SessionUser, error) {
	var u SessionUser
	err := s.DB.QueryRow(`SELECT id, username, display_name, is_active, role_version FROM users WHERE id=?`,
		userID).Scan(&u.ID, &u.Username, &u.DisplayName, &u.IsActive, &u.RoleVersion)
	if err != nil {
		return u, err
	}
	if !u.IsActive {
		return u, ErrInactive
	}
	if u.RoleVersion != rv {
		return u, ErrStaleToken
	}
	rows, err := s.DB.Query(`SELECT r.name FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=?`, userID)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return u, err
		}
		u.Roles = append(u.Roles, name)
	}
	return u, rows.Err()
}

// BumpUser 使用户现有 token 全部失效。
func (s *Service) BumpUser(userID int64) error {
	_, err := s.DB.Exec(`UPDATE users SET role_version=role_version+1 WHERE id=?`, userID)
	return err
}

// Can 判定用户是否拥有权限码 code：全局权限直接放行；
// instanceID>0 时还需该实例的 grant（用户级或角色级）。
func (s *Service) Can(userID int64, code string, instanceID int64) (bool, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM user_roles ur
		JOIN role_permissions rp ON rp.role_id=ur.role_id
		WHERE ur.user_id=? AND rp.code=?`, userID, code).Scan(&n)
	if err != nil || n == 0 {
		return false, err
	}
	if instanceID == 0 {
		return true, nil // 全局权限
	}
	err = s.DB.QueryRow(`SELECT COUNT(*) FROM instance_grants ig
		WHERE ig.instance_id=? AND (ig.user_id=? OR ig.role_id IN
			(SELECT role_id FROM user_roles WHERE user_id=?))`,
		instanceID, userID, userID).Scan(&n)
	return n > 0, err
}

// BumpRole 使该角色全部用户的现有 token 失效。
func (s *Service) BumpRole(roleID int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET role_version=role_version+1 WHERE id IN
		(SELECT user_id FROM user_roles WHERE role_id=?)`, roleID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceUserRoles 整体替换用户的角色并 bump 其 role_version。
func (s *Service) ReplaceUserRoles(userID int64, roleIDs []int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM user_roles WHERE user_id=?`, userID); err != nil {
		return err
	}
	for _, rid := range roleIDs {
		if _, err := tx.Exec(`INSERT INTO user_roles(user_id, role_id) VALUES(?,?)`, userID, rid); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE users SET role_version=role_version+1 WHERE id=?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceUserGrants 整体替换用户的实例授权并 bump 其 role_version。
func (s *Service) ReplaceUserGrants(userID int64, instanceIDs []int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM instance_grants WHERE user_id=?`, userID); err != nil {
		return err
	}
	for _, iid := range instanceIDs {
		if _, err := tx.Exec(`INSERT INTO instance_grants(user_id, instance_id) VALUES(?,?)`, userID, iid); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE users SET role_version=role_version+1 WHERE id=?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceRolePermissions 整体替换角色的权限码，并 bump 该角色全部用户。
func (s *Service) ReplaceRolePermissions(roleID int64, codes []string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM role_permissions WHERE role_id=?`, roleID); err != nil {
		return err
	}
	for _, code := range codes {
		if _, err := tx.Exec(`INSERT INTO role_permissions(role_id, code) VALUES(?,?)`, roleID, code); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE users SET role_version=role_version+1 WHERE id IN
		(SELECT user_id FROM user_roles WHERE role_id=?)`, roleID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceRoleGrants 整体替换角色的实例授权，并 bump 该角色全部用户。
func (s *Service) ReplaceRoleGrants(roleID int64, instanceIDs []int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM instance_grants WHERE role_id=?`, roleID); err != nil {
		return err
	}
	for _, iid := range instanceIDs {
		if _, err := tx.Exec(`INSERT INTO instance_grants(role_id, instance_id) VALUES(?,?)`, roleID, iid); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE users SET role_version=role_version+1 WHERE id IN
		(SELECT user_id FROM user_roles WHERE role_id=?)`, roleID); err != nil {
		return err
	}
	return tx.Commit()
}
