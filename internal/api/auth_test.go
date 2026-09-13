package api

import (
	"net/http"
	"testing"
)

func TestLoginAndMe(t *testing.T) {
	r, _ := newTestRouter(t)
	postJSON(r, "/api/v1/setup", "", map[string]string{"username": "root", "password": "good-pass-1"})
	w := postJSON(r, "/api/v1/login", "", map[string]string{"username": "root", "password": "good-pass-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	token := decode(t, w.Body.Bytes())["token"].(string)

	// /me 需要带 token
	if w = getJSON(r, "/api/v1/me", token); w.Code != http.StatusOK {
		t.Fatalf("me: %d %s", w.Code, w.Body.String())
	}
	// 无 token 401
	if w = getJSON(r, "/api/v1/me", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token want 401 got %d", w.Code)
	}
	// 错误密码 401
	if w = postJSON(r, "/api/v1/login", "", map[string]string{"username": "root", "password": "nope-nope"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad login want 401 got %d", w.Code)
	}
}
