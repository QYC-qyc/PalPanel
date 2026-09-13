package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"palpanel/internal/audit"
	"palpanel/internal/auth"
	"palpanel/internal/config"
	"palpanel/internal/db"
)

func newTestRouter(t *testing.T) (*gin.Engine, *auth.Service) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	if err := auth.Seed(d); err != nil {
		t.Fatal(err)
	}
	secret, err := auth.LoadOrCreateSecret(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.New(d, secret)
	return New(Deps{
		Cfg:    config.Config{Listen: ":0", DataDir: dir},
		DB:     d,
		Auth:   svc,
		Audit:  audit.New(d),
		Secret: secret,
	}), svc
}

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return m
}

func doJSON(r *gin.Engine, method, path, token string, body any) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r.ServeHTTP(w, req)
	return w
}

func getJSON(r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	return doJSON(r, "GET", path, token, nil)
}

func postJSON(r *gin.Engine, path, token string, body any) *httptest.ResponseRecorder {
	return doJSON(r, "POST", path, token, body)
}

func patchJSON(r *gin.Engine, path, token string, body any) *httptest.ResponseRecorder {
	return doJSON(r, "PATCH", path, token, body)
}

func putJSON(r *gin.Engine, path, token string, body any) *httptest.ResponseRecorder {
	return doJSON(r, "PUT", path, token, body)
}

func deleteJSON(r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	return doJSON(r, "DELETE", path, token, nil)
}

// loginToken 用给定账号登录并返回 token。
func loginToken(t *testing.T, r *gin.Engine, username, password string) string {
	t.Helper()
	w := postJSON(r, "/api/v1/login", "", map[string]string{"username": username, "password": password})
	if w.Code != 200 {
		t.Fatalf("login %s: %d %s", username, w.Code, w.Body.String())
	}
	return decode(t, w.Body.Bytes())["token"].(string)
}

// setupAdmin 完成 setup + login，返回路由、auth 服务与管理员 token。
func setupAdmin(t *testing.T) (*gin.Engine, *auth.Service, string) {
	t.Helper()
	r, svc := newTestRouter(t)
	postJSON(r, "/api/v1/setup", "", map[string]string{"username": "root", "password": "good-pass-1"})
	return r, svc, loginToken(t, r, "root", "good-pass-1")
}

// operatorRoleID 直接查库取 operator 角色 ID（roles API 属后续任务）。
func operatorRoleID(t *testing.T, svc *auth.Service) []int64 {
	t.Helper()
	var id int64
	if err := svc.DB.QueryRow(`SELECT id FROM roles WHERE name='operator'`).Scan(&id); err != nil {
		t.Fatalf("query operator role: %v", err)
	}
	return []int64{id}
}
