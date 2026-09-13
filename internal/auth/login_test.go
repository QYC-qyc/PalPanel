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
