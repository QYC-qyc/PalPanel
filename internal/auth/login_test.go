package auth

import (
	"errors"
	"testing"

	"palpanel/internal/db"
)

func newService(t *testing.T) *Service {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	if err := Seed(d); err != nil {
		t.Fatal(err)
	}
	key, err := LoadOrCreateSecret(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(d, key)
}

func TestLoginFlow(t *testing.T) {
	s := newService(t)
	if _, err := s.Setup("root", "good-pass-1"); err != nil {
		t.Fatal(err)
	}
	token, err := s.Login("root", "good-pass-1")
	if err != nil {
		t.Fatal(err)
	}

	// 用不同 secret 解析必须失败
	if _, _, err := ParseToken([]byte("wrong-secret-0123456789abcdef"), token); err == nil {
		t.Fatal("wrong secret should fail")
	}

	// 用同一 secret 解析成功，并取到 SessionUser
	uid, rv, err := ParseToken(s.Secret, token)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.SessionUser(uid, rv)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != uid || u.Username != "root" || !u.IsActive || u.RoleVersion != rv ||
		len(u.Roles) != 1 || u.Roles[0] != "admin" {
		t.Fatalf("session user wrong: %+v", u)
	}

	if _, err := s.Login("root", "wrong-pass!"); !errors.Is(err, ErrBadCreds) {
		t.Fatalf("want ErrBadCreds got %v", err)
	}
}

// TestLoginNoUserEnum：登录失败的三种情形（用户不存在/密码错误/账号停用）
// 必须返回同一错误 ErrBadCreds，防止用户枚举；ErrInactive 仅保留给 SessionUser。
func TestLoginNoUserEnum(t *testing.T) {
	s := newService(t)
	if _, err := s.Setup("root", "good-pass-1"); err != nil {
		t.Fatal(err)
	}

	_, errNoUser := s.Login("no-such-user", "whatever-1")
	_, errBadPass := s.Login("root", "wrong-pass!")
	if !errors.Is(errNoUser, ErrBadCreds) || !errors.Is(errBadPass, ErrBadCreds) {
		t.Fatalf("want ErrBadCreds, got %v / %v", errNoUser, errBadPass)
	}
	if errNoUser.Error() != errBadPass.Error() {
		t.Fatalf("error text mismatch: %q vs %q", errNoUser.Error(), errBadPass.Error())
	}

	if _, err := s.DB.Exec(`UPDATE users SET is_active=0 WHERE username='root'`); err != nil {
		t.Fatal(err)
	}
	_, errInactive := s.Login("root", "good-pass-1")
	if !errors.Is(errInactive, ErrBadCreds) {
		t.Fatalf("inactive login want ErrBadCreds got %v", errInactive)
	}
	if errors.Is(errInactive, ErrInactive) {
		t.Fatal("inactive login must not leak ErrInactive")
	}
}

func TestStaleTokenRejected(t *testing.T) {
	s := newService(t)
	if _, err := s.Setup("root", "good-pass-1"); err != nil {
		t.Fatal(err)
	}
	token, err := s.Login("root", "good-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	uid, rv, err := ParseToken(s.Secret, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BumpUser(uid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(uid, rv); !errors.Is(err, ErrStaleToken) {
		t.Fatalf("want ErrStaleToken got %v", err)
	}
}
