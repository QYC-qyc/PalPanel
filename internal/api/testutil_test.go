package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"palpanel/internal/audit"
	"palpanel/internal/auth"
	"palpanel/internal/config"
	"palpanel/internal/db"
	"palpanel/internal/event"
	"palpanel/internal/gateway"
	"palpanel/internal/instance"
	"palpanel/internal/job"
	"palpanel/internal/rcon"
	"palpanel/internal/supervisor"
)

// ---- fake 进程与守护器注入（零真进程） ----

type fakeProc struct {
	mu   sync.Mutex
	done chan struct{}
}

func newFakeProc() *fakeProc { return &fakeProc{done: make(chan struct{})} }

func (p *fakeProc) Pid() int { return 4242 }

func (p *fakeProc) Wait() error { <-p.done; return nil }

func (p *fakeProc) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
	default:
		close(p.done)
	}
	return nil
}

type fakeProber struct{}

func (fakeProber) Probe(ctx context.Context, inst instance.Instance) error { return nil }

// newTestDeps 返回路由与完整 Deps（含 Hub/Jobs/Sup，供 WS、实例动作等测试按需取用）。
// Sup 的 StartFn/Killer/Prober 均为 fake：start 立即 running、stop 经 Killer 让 Wait 返回；
// RESTFor/RCONFor 默认为报错 stub，测试按需在 cfg 里替换为 fake 网关。
func newTestDeps(t *testing.T) (*gin.Engine, Deps) {
	return newTestDepsCfg(t, nil)
}

// newTestDepsCfg 在构造路由前允许测试改写 Deps（注入 fake RESTFor/Installer 等），
// 避免 New(deps) 值拷贝导致路由侧看不到后续修改。
func newTestDepsCfg(t *testing.T, cfg func(*Deps)) (*gin.Engine, Deps) {
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
	hub := event.NewHub()

	var mu sync.Mutex
	var cur *fakeProc
	sup := supervisor.New(d, instance.New(d), hub)
	sup.LogRoot = dir
	// fake Sup 的停机宽限期调短：默认 RCONFor stub 恒失败，supervisor 会等满宽限期再强杀
	sup.StopGraceWait = 300 * time.Millisecond
	sup.Killer = func(pid int) error {
		mu.Lock()
		p := cur
		mu.Unlock()
		if p != nil {
			return p.Kill() // 模拟 taskkill：Wait 随之返回
		}
		return nil
	}
	sup.StartFn = func(inst instance.Instance, args []string) (supervisor.RunningProcess, io.ReadCloser, error) {
		p := newFakeProc()
		mu.Lock()
		cur = p
		mu.Unlock()
		return p, io.NopCloser(strings.NewReader("")), nil
	}
	sup.Prober = fakeProber{}

	deps := Deps{
		Cfg:       config.Config{Listen: ":0", DataDir: dir},
		DB:        d,
		Auth:      auth.New(d, secret),
		Audit:     audit.New(d),
		Secret:    secret,
		Instances: instance.New(d),
		Hub:       hub,
		Jobs:      job.NewManager(hub),
		Sup:       sup,
		RESTFor: func(instance.Instance) (*gateway.Client, error) {
			return nil, errors.New("REST 未注入")
		},
		RCONFor: func(instance.Instance) (*rcon.Conn, error) {
			return nil, errors.New("RCON 未注入")
		},
	}
	if cfg != nil {
		cfg(&deps)
	}
	return New(deps), deps
}

// newTestRouter 返回路由、auth 服务与数据库句柄（测试按需取用）。
func newTestRouter(t *testing.T) (*gin.Engine, *auth.Service, *sql.DB) {
	t.Helper()
	r, deps := newTestDeps(t)
	return r, deps.Auth, deps.DB
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

// setupAdminDeps：setup + login，返回路由、完整 Deps 与管理员 token。
func setupAdminDeps(t *testing.T) (*gin.Engine, Deps, string) {
	t.Helper()
	r, deps := newTestDeps(t)
	postJSON(r, "/api/v1/setup", "", map[string]string{"username": "root", "password": "good-pass-1"})
	return r, deps, loginToken(t, r, "root", "good-pass-1")
}

// setupAdminCfg：同 setupAdminDeps，但允许测试在构造路由前注入 Deps 定制。
func setupAdminCfg(t *testing.T, cfg func(*Deps)) (*gin.Engine, Deps, string) {
	t.Helper()
	r, deps := newTestDepsCfg(t, cfg)
	postJSON(r, "/api/v1/setup", "", map[string]string{"username": "root", "password": "good-pass-1"})
	return r, deps, loginToken(t, r, "root", "good-pass-1")
}

// createInstance 经 API 创建实例并返回 ID（默认端口 8211/27015）。
func createInstance(t *testing.T, r *gin.Engine, token, gameDir string) int64 {
	t.Helper()
	w := postJSON(r, "/api/v1/instances", token, map[string]any{
		"name": "s1", "game_dir": gameDir, "game_port": 8211,
		"query_port": 27015, "admin_password": "secret-pw-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("create instance: %d %s", w.Code, w.Body.String())
	}
	return int64(decode(t, w.Body.Bytes())["id"].(float64))
}

// waitStatus 轮询等待 Sup 上实例达到目标状态。
func waitStatus(t *testing.T, deps Deps, id int64, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if st, ok := deps.Sup.Status(id); ok && st.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	st, ok := deps.Sup.Status(id)
	t.Fatalf("状态未到达 %q：Status=(%+v,%v)", want, st, ok)
}

// waitJobState 轮询等待任务达到目标状态。
func waitJobState(t *testing.T, deps Deps, jobID, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if j, ok := deps.Jobs.Get(jobID); ok && j.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	j, _ := deps.Jobs.Get(jobID)
	t.Fatalf("任务未到达 %q：Job=%+v", want, j)
}
