package api

import (
	"net/http"
	"testing"
)

func TestSetupHTTP(t *testing.T) {
	r, _ := newTestRouter(t)
	if w := postJSON(r, "/api/v1/setup", "", map[string]string{"username": "root", "password": "good-pass-1"}); w.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	if w := postJSON(r, "/api/v1/setup", "", map[string]string{"username": "root2", "password": "good-pass-2"}); w.Code != http.StatusForbidden {
		t.Fatalf("second setup should 403: %d", w.Code)
	}
	// 审计已记录 auth.setup
}
