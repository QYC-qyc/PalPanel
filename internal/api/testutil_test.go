package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"palpanel/internal/audit"
	"palpanel/internal/auth"
	"palpanel/internal/config"
	"palpanel/internal/db"
	"palpanel/internal/instance"
)

// newTestRouter 返回路由、auth 服务与数据库句柄（测试按需取用）。
func newTestRouter(t *testing.T) (*gin.Engine, *auth.Service, *sql.DB) {
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
	r := New(Deps{
		Cfg:       config.Config{Listen: ":0", DataDir: dir},
		DB:        d,
		Auth:      svc,
		Audit:     audit.New(d),
		Secret:    secret,
		Instances: instance.New(d),
	})
	return r, svc, d
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

// setupAdmin 完成 setup + login，返回路由、数据库句柄与管理员 token。
func setupAdmin(t *testing.T) (*gin.Engine, *sql.DB, string) {
	t.Helper()
	r, _, d := newTestRouter(t)
	postJSON(r, "/api/v1/setup", "", map[string]string{"username": "root", "password": "good-pass-1"})
	return r, d, loginToken(t, r, "root", "good-pass-1")
}

// queryInt64 查询单值（不存在时报错，避免测试里吞错）。
func queryInt64(t *testing.T, d *sql.DB, query string) int64 {
	t.Helper()
	var id int64
	if err := d.QueryRow(query).Scan(&id); err != nil {
		t.Fatalf("query %s: %v", query, err)
	}
	return id
}

// countRows 统计满足条件的行数（用于断言级联清理无残留）。
func countRows(t *testing.T, d *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := d.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", query, err)
	}
	return n
}

// operatorRoleID 直接查库取 operator 角色 ID（内置角色 ID 不硬编码）。
func operatorRoleID(t *testing.T, d *sql.DB) []int64 {
	t.Helper()
	return []int64{queryInt64(t, d, `SELECT id FROM roles WHERE name='operator'`)}
}
