package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"palpanel/internal/gateway"
	"palpanel/internal/instance"
	"palpanel/internal/supervisor"
)

// fakeInstaller 实现 api.Installer 接口（生产为 *installer.Service）。
type fakeInstaller struct {
	installFn func(ctx context.Context, gameDir string, report func(int, string), onLine func(string)) error
	checkFn   func(ctx context.Context, gameDir string, onLine func(string)) (string, string, error)
}

func (f fakeInstaller) Install(ctx context.Context, gameDir string, report func(int, string), onLine func(string)) error {
	if f.installFn == nil {
		return nil
	}
	return f.installFn(ctx, gameDir, report, onLine)
}

func (f fakeInstaller) UpdateCheck(ctx context.Context, gameDir string, onLine func(string)) (string, string, error) {
	if f.checkFn == nil {
		return "100", "100", nil
	}
	return f.checkFn(ctx, gameDir, onLine)
}

// fakeRESTServer 起一个自签 TLS 的游戏服 REST 伪装端点（gateway 客户端 InsecureSkipVerify）。
type fakeRESTServer struct {
	srv *httptest.Server

	mu        sync.Mutex
	kicked    []string
	banned    []string
	announce  []string
	saves     int
	shutdowns int
}

func newFakeRESTServer(players string) *fakeRESTServer {
	f := &fakeRESTServer{}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v1/api/players":
			_, _ = w.Write([]byte(players))
		case "/v1/api/kick":
			var body struct {
				UserID string `json:"userid"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			f.mu.Lock()
			f.kicked = append(f.kicked, body.UserID)
			f.mu.Unlock()
			_, _ = w.Write([]byte("{}"))
		case "/v1/api/ban":
			var body struct {
				UserID string `json:"userid"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			f.mu.Lock()
			f.banned = append(f.banned, body.UserID)
			f.mu.Unlock()
			_, _ = w.Write([]byte("{}"))
		case "/v1/api/announce":
			var body struct {
				Message string `json:"message"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			f.mu.Lock()
			f.announce = append(f.announce, body.Message)
			f.mu.Unlock()
			_, _ = w.Write([]byte("{}"))
		case "/v1/api/save":
			f.mu.Lock()
			f.saves++
			f.mu.Unlock()
			_, _ = w.Write([]byte("{}"))
		case "/v1/api/shutdown":
			f.mu.Lock()
			f.shutdowns++
			f.mu.Unlock()
			_, _ = w.Write([]byte("{}"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return f
}

func TestStartStopAPI(t *testing.T) {
	r, deps, admin := setupAdminDeps(t)
	id := createInstance(t, r, admin, t.TempDir())

	// start → 200 → running
	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/start", admin, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	waitStatus(t, deps, id, supervisor.StateRunning, 3*time.Second)

	// status：running + fake pid
	w = getJSON(r, "/api/v1/instances/"+itoa(id)+"/status", admin)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	m := decode(t, w.Body.Bytes())
	if m["status"] != "running" {
		t.Fatalf("status = %v", m["status"])
	}
	if pid, _ := m["pid"].(float64); pid != 4242 {
		t.Fatalf("pid = %v", m["pid"])
	}
	if _, ok := m["started_at"]; !ok {
		t.Fatal("started_at missing")
	}

	// 重复 start → 409
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/start", admin, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate start: %d %s", w.Code, w.Body.String())
	}

	// stop → 200 → idle
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/stop", admin, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", w.Code, w.Body.String())
	}
	waitStatus(t, deps, id, supervisor.StateIdle, 3*time.Second)

	// restart（idle 状态重启）：stop 幂等容忍 → start → running
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/restart", admin, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("restart: %d %s", w.Code, w.Body.String())
	}
	waitStatus(t, deps, id, supervisor.StateRunning, 3*time.Second)

	// 清理：避免 goroutine 残留影响其他测试
	postJSON(r, "/api/v1/instances/"+itoa(id)+"/stop", admin, nil)
	waitStatus(t, deps, id, supervisor.StateIdle, 3*time.Second)

	// 不存在的实例 → 404
	w = postJSON(r, "/api/v1/instances/999/start", admin, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("start missing: %d", w.Code)
	}
}

func TestPlayersKickPermission(t *testing.T) {
	fake := newFakeRESTServer(`[{"name":"p1","uid":"1","steamid":"76561198","ping":12.5}]`)
	defer fake.srv.Close()
	r, deps, admin := setupAdminCfg(t, func(d *Deps) {
		d.RESTFor = func(instance.Instance) (*gateway.Client, error) {
			return gateway.NewForTest(fake.srv.URL, "pw"), nil
		}
	})
	id := createInstance(t, r, admin, t.TempDir())

	// viewer 用户：player.read + 实例 grant，无 player.kick
	roleID := queryInt64(t, deps.DB, `SELECT id FROM roles WHERE name='viewer'`)
	w := postJSON(r, "/api/v1/users", admin, map[string]any{
		"username": "obs", "password": "viewer-pass-1", "role_ids": []int64{roleID}})
	if w.Code != http.StatusOK {
		t.Fatalf("create viewer: %d %s", w.Code, w.Body.String())
	}
	uid := int64(decode(t, w.Body.Bytes())["id"].(float64))
	w = putJSON(r, fmt.Sprintf("/api/v1/users/%d/grants", uid), admin,
		map[string]any{"instance_ids": []int64{id}})
	if w.Code != http.StatusOK {
		t.Fatalf("grants: %d %s", w.Code, w.Body.String())
	}
	viewer := loginToken(t, r, "obs", "viewer-pass-1")

	// GET players → 200，返回 fake 玩家
	w = getJSON(r, "/api/v1/instances/"+itoa(id)+"/players", viewer)
	if w.Code != http.StatusOK {
		t.Fatalf("players: %d %s", w.Code, w.Body.String())
	}
	ps := decode(t, w.Body.Bytes())["players"].([]any)
	if len(ps) != 1 || ps[0].(map[string]any)["name"] != "p1" {
		t.Fatalf("players = %s", w.Body.String())
	}

	// viewer POST kick → 403
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/players/kick", viewer, map[string]string{"uid": "1"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer kick: %d %s", w.Code, w.Body.String())
	}

	// admin kick/ban/announce/save → 200 且到达 fake 网关
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/players/kick", admin, map[string]string{"uid": "1"})
	if w.Code != http.StatusOK {
		t.Fatalf("admin kick: %d %s", w.Code, w.Body.String())
	}
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/players/ban", admin, map[string]string{"uid": "2"})
	if w.Code != http.StatusOK {
		t.Fatalf("admin ban: %d %s", w.Code, w.Body.String())
	}
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/announce", admin, map[string]string{"message": "hello"})
	if w.Code != http.StatusOK {
		t.Fatalf("announce: %d %s", w.Code, w.Body.String())
	}
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/save", admin, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.kicked) != 1 || fake.kicked[0] != "1" {
		t.Fatalf("kicked = %v", fake.kicked)
	}
	if len(fake.banned) != 1 || fake.banned[0] != "2" {
		t.Fatalf("banned = %v", fake.banned)
	}
	if len(fake.announce) != 1 || fake.announce[0] != "hello" {
		t.Fatalf("announce = %v", fake.announce)
	}
	if fake.saves != 1 {
		t.Fatalf("saves = %d", fake.saves)
	}
}

func TestInstallDuplicateConflict(t *testing.T) {
	block := make(chan struct{})
	var calls atomic.Int32
	r, deps, admin := setupAdminCfg(t, func(d *Deps) {
		d.Installer = fakeInstaller{installFn: func(ctx context.Context, gameDir string, report func(int, string), onLine func(string)) error {
			calls.Add(1)
			<-block
			return nil
		}}
	})
	id := createInstance(t, r, admin, t.TempDir())

	// 第一个 install → 200（异步进行中）
	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/install", admin, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("install: %d %s", w.Code, w.Body.String())
	}
	// 进行中重复 install → 409
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/install", admin, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate install: %d %s", w.Code, w.Body.String())
	}
	close(block)
	deadline := time.Now().Add(2 * time.Second)
	for {
		jobs := deps.Jobs.List()
		if len(jobs) == 1 && jobs[0].State == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("install job not done: %+v", jobs)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("install calls = %d", calls.Load())
	}
}

func TestUpdateCheckEndpoint(t *testing.T) {
	r, _, admin := setupAdminCfg(t, func(d *Deps) {
		d.Installer = fakeInstaller{checkFn: func(ctx context.Context, gameDir string, onLine func(string)) (string, string, error) {
			return "100", "101", nil
		}}
	})
	id := createInstance(t, r, admin, t.TempDir())

	w := getJSON(r, "/api/v1/instances/"+itoa(id)+"/update-check", admin)
	if w.Code != http.StatusOK {
		t.Fatalf("update-check: %d %s", w.Code, w.Body.String())
	}
	m := decode(t, w.Body.Bytes())
	if m["local"] != "100" || m["remote"] != "101" || m["up_to_date"] != false {
		t.Fatalf("update-check = %s", w.Body.String())
	}
}

// TestUpdateStopsRunningInstance：运行中实例发起更新 → 先优雅停服 → 再安装 → 保持停止。
func TestUpdateStopsRunningInstance(t *testing.T) {
	installCalled := make(chan struct{})
	r, deps, admin := setupAdminCfg(t, func(d *Deps) {
		d.Installer = fakeInstaller{installFn: func(ctx context.Context, gameDir string, report func(int, string), onLine func(string)) error {
			close(installCalled)
			return nil
		}}
	})
	id := createInstance(t, r, admin, t.TempDir())

	postJSON(r, "/api/v1/instances/"+itoa(id)+"/start", admin, nil)
	waitStatus(t, deps, id, supervisor.StateRunning, 3*time.Second)

	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/update", admin, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-installCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("install not invoked")
	}
	waitStatus(t, deps, id, supervisor.StateIdle, 3*time.Second) // 更新后保持停止
	waitJobState(t, deps, "job-1", "done", 3*time.Second)
}

// TestUpdateStopsOrphanRunningInstance：面板外残留进程——守护器无托管记录
//（Sup 无 proc）但 DB 状态为 running——update 同样先走优雅停机链（REST Shutdown）再更新。
func TestUpdateStopsOrphanRunningInstance(t *testing.T) {
	fake := newFakeRESTServer(`[]`)
	installCalled := make(chan struct{})
	r, deps, admin := setupAdminCfg(t, func(d *Deps) {
		d.RESTFor = func(instance.Instance) (*gateway.Client, error) {
			return gateway.NewForTest(fake.srv.URL, "pw"), nil
		}
		d.Installer = fakeInstaller{installFn: func(ctx context.Context, gameDir string, report func(int, string), onLine func(string)) error {
			close(installCalled)
			return nil
		}}
	})
	id := createInstance(t, r, admin, t.TempDir())

	// 面板外残留：Sup 无托管进程，仅 DB 状态为 running
	if _, err := deps.DB.Exec(`UPDATE instances SET status='running' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}

	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/update", admin, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-installCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("install not invoked")
	}
	waitJobState(t, deps, "job-1", "done", 3*time.Second)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.shutdowns != 1 {
		t.Fatalf("残留进程停服链未被调用：shutdowns = %d", fake.shutdowns)
	}
}

// TestStartBlockedDuringUpdate：update job 运行中 start → 409；job 完成后 start → 200。
func TestStartBlockedDuringUpdate(t *testing.T) {
	release := make(chan struct{})
	installEntered := make(chan struct{})
	r, deps, admin := setupAdminCfg(t, func(d *Deps) {
		d.Installer = fakeInstaller{installFn: func(ctx context.Context, gameDir string, report func(int, string), onLine func(string)) error {
			close(installEntered)
			<-release
			return nil
		}}
	})
	id := createInstance(t, r, admin, t.TempDir())

	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/update", admin, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-installEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("update job 未进入安装阶段")
	}

	// update job 存续期间：start → 409
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/start", admin, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("start during update: %d %s", w.Code, w.Body.String())
	}

	close(release)
	waitJobState(t, deps, "job-1", "done", 3*time.Second)

	// job 完成后：start → 200
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/start", admin, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("start after update: %d %s", w.Code, w.Body.String())
	}
	waitStatus(t, deps, id, supervisor.StateRunning, 3*time.Second)
}

func TestLogsTail(t *testing.T) {
	r, deps, admin := setupAdminDeps(t)
	id := createInstance(t, r, admin, t.TempDir())

	// console.log 存在：tail=2 取末 2 行
	logDir := filepath.Join(deps.Cfg.DataDir, "instances", itoa(id), "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "console.log"), []byte("l1\nl2\nl3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := getJSON(r, "/api/v1/instances/"+itoa(id)+"/logs?tail=2", admin)
	if w.Code != http.StatusOK {
		t.Fatalf("logs: %d %s", w.Code, w.Body.String())
	}
	lines := decode(t, w.Body.Bytes())["lines"].([]any)
	if len(lines) != 2 || lines[0] != "l2" || lines[1] != "l3" {
		t.Fatalf("lines = %s", w.Body.String())
	}

	// 文件缺失 → 空数组不报错
	id2 := createInstance2(t, r, admin)
	w = getJSON(r, "/api/v1/instances/"+itoa(id2)+"/logs", admin)
	if w.Code != http.StatusOK {
		t.Fatalf("logs missing file: %d %s", w.Code, w.Body.String())
	}
	lines = decode(t, w.Body.Bytes())["lines"].([]any)
	if len(lines) != 0 {
		t.Fatalf("lines = %s", w.Body.String())
	}
}

// createInstance2 用不同端口再建一个实例（避免端口冲突）。
func createInstance2(t *testing.T, r *gin.Engine, token string) int64 {
	t.Helper()
	w := postJSON(r, "/api/v1/instances", token, map[string]any{
		"name": "s2", "game_dir": t.TempDir(), "game_port": 8212,
		"query_port": 27016, "admin_password": "secret-pw-2"})
	if w.Code != http.StatusOK {
		t.Fatalf("create instance2: %d %s", w.Code, w.Body.String())
	}
	return int64(decode(t, w.Body.Bytes())["id"].(float64))
}
