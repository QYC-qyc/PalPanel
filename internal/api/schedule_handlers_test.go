package api

import (
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// fakeSched 记录 Reload 调用次数（实现 api.ScheduleReloader）。
type fakeSched struct {
	mu      sync.Mutex
	reloadN int
}

func (f *fakeSched) Reload() {
	f.mu.Lock()
	f.reloadN++
	f.mu.Unlock()
}

func (f *fakeSched) Reloads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reloadN
}

// newScheduleEnv 构造带 fake 调度器（Reload 计数）的路由。
func newScheduleEnv(t *testing.T) (*gin.Engine, Deps, *fakeSched, string) {
	t.Helper()
	fs := &fakeSched{}
	r, deps, token := setupAdminCfg(t, func(d *Deps) {
		d.Sched = fs
	})
	return r, deps, fs, token
}

func TestScheduleCreateList(t *testing.T) {
	r, deps, fs, token := newScheduleEnv(t)
	id := createInstance(t, r, token, t.TempDir())

	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/schedules", token, map[string]any{
		"kind": "backup", "cron_expr": "0 3 * * *", "payload": map[string]any{"keep": 2}})
	if w.Code != http.StatusOK {
		t.Fatalf("POST schedules: %d %s", w.Code, w.Body.String())
	}
	sid := int64(decode(t, w.Body.Bytes())["id"].(float64))
	if fs.Reloads() != 1 {
		t.Fatalf("创建后应 Reload 1 次，实际 %d", fs.Reloads())
	}

	// last_run_at 预置为 now（规避新行补跑），next_run_at 留空交给 Reload 修复
	if got := countRows(t, deps.DB, `SELECT COUNT(*) FROM schedules WHERE id=? AND last_run_at IS NOT NULL AND next_run_at IS NULL`, sid); got != 1 {
		t.Fatalf("last_run_at 应预置且 next_run_at 为空: %d", got)
	}

	w = getJSON(r, "/api/v1/instances/"+itoa(id)+"/schedules", token)
	if w.Code != http.StatusOK {
		t.Fatalf("GET schedules: %d %s", w.Code, w.Body.String())
	}
	list, _ := decode(t, w.Body.Bytes())["schedules"].([]any)
	if len(list) != 1 {
		t.Fatalf("期望 1 条 schedule: %s", w.Body.String())
	}
	row := list[0].(map[string]any)
	if row["kind"] != "backup" || row["cron_expr"] != "0 3 * * *" {
		t.Fatalf("kind/cron_expr 不符: %v", row)
	}
	payload, ok := row["payload"].(map[string]any)
	if !ok || payload["keep"].(float64) != 2 {
		t.Fatalf("payload 应原样返回对象: %v", row["payload"])
	}
	if row["enabled"] != true {
		t.Fatalf("默认 enabled 应为 true: %v", row)
	}
}

func TestScheduleValidation(t *testing.T) {
	r, deps, fs, token := newScheduleEnv(t)
	id := createInstance(t, r, token, t.TempDir())

	// 非法 cron → 400
	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/schedules", token,
		map[string]any{"kind": "backup", "cron_expr": "not-a-cron"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 cron 应 400: %d %s", w.Code, w.Body.String())
	}
	// 非法 kind → 400
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/schedules", token,
		map[string]any{"kind": "reboot", "cron_expr": "0 3 * * *"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 kind 应 400: %d", w.Code)
	}
	// 缺 cron_expr → 400
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/schedules", token,
		map[string]any{"kind": "backup"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 cron_expr 应 400: %d", w.Code)
	}
	if n := countRows(t, deps.DB, `SELECT COUNT(*) FROM schedules`); n != 0 {
		t.Fatalf("非法请求不应落库: %d", n)
	}
	if fs.Reloads() != 0 {
		t.Fatalf("非法请求不应触发 Reload: %d", fs.Reloads())
	}
}

func TestSchedulePatchDelete(t *testing.T) {
	r, deps, fs, token := newScheduleEnv(t)
	id := createInstance(t, r, token, t.TempDir())

	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/schedules", token,
		map[string]any{"kind": "backup", "cron_expr": "0 3 * * *"})
	sid := int64(decode(t, w.Body.Bytes())["id"].(float64))

	// PATCH 换 cron + 停用
	w = patchJSON(r, "/api/v1/instances/"+itoa(id)+"/schedules/"+itoa(sid), token,
		map[string]any{"cron_expr": "30 4 * * 1", "enabled": false})
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH schedule: %d %s", w.Code, w.Body.String())
	}
	if fs.Reloads() != 2 {
		t.Fatalf("PATCH 后应 Reload（共 2 次），实际 %d", fs.Reloads())
	}
	// 改动生效；next_run_at 被清空以便 Reload 重锚
	var enabled int64
	var next any
	if err := deps.DB.QueryRow(`SELECT enabled, next_run_at FROM schedules WHERE id=?`, sid).Scan(&enabled, &next); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 || next != nil {
		t.Fatalf("PATCH 后 enabled=0 且 next_run_at=NULL，得到 %d %v", enabled, next)
	}
	// PATCH 非法 cron → 400 且不 Reload
	w = patchJSON(r, "/api/v1/instances/"+itoa(id)+"/schedules/"+itoa(sid), token,
		map[string]any{"cron_expr": "bad"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PATCH 非法 cron 应 400: %d", w.Code)
	}
	if fs.Reloads() != 2 {
		t.Fatalf("非法 PATCH 不应 Reload: %d", fs.Reloads())
	}

	// DELETE
	w = deleteJSON(r, "/api/v1/instances/"+itoa(id)+"/schedules/"+itoa(sid), token)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE schedule: %d %s", w.Code, w.Body.String())
	}
	if n := countRows(t, deps.DB, `SELECT COUNT(*) FROM schedules WHERE id=?`, sid); n != 0 {
		t.Fatalf("schedule 行应已删除")
	}
	if fs.Reloads() != 3 {
		t.Fatalf("DELETE 后应 Reload（共 3 次），实际 %d", fs.Reloads())
	}
	// 再删 → 404
	w = deleteJSON(r, "/api/v1/instances/"+itoa(id)+"/schedules/"+itoa(sid), token)
	if w.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404: %d", w.Code)
	}
}

func TestScheduleCrossInstanceIsolation(t *testing.T) {
	r, _, _, token := newScheduleEnv(t)
	id1 := createInstance(t, r, token, t.TempDir())
	// 第二个实例端口须错开（createInstance 固定 8211/27015）
	w := postJSON(r, "/api/v1/instances", token, map[string]any{
		"name": "s2", "game_dir": t.TempDir(), "game_port": 8212,
		"query_port": 27016, "admin_password": "secret-pw-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("create instance 2: %d %s", w.Code, w.Body.String())
	}
	id2 := int64(decode(t, w.Body.Bytes())["id"].(float64))

	w = postJSON(r, "/api/v1/instances/"+itoa(id1)+"/schedules", token,
		map[string]any{"kind": "backup", "cron_expr": "0 3 * * *"})
	sid := int64(decode(t, w.Body.Bytes())["id"].(float64))

	// 经实例 2 的路径访问实例 1 的 schedule → 404
	if w := patchJSON(r, "/api/v1/instances/"+itoa(id2)+"/schedules/"+itoa(sid), token,
		map[string]any{"enabled": false}); w.Code != http.StatusNotFound {
		t.Fatalf("跨实例 PATCH 应 404: %d", w.Code)
	}
	if w := deleteJSON(r, "/api/v1/instances/"+itoa(id2)+"/schedules/"+itoa(sid), token); w.Code != http.StatusNotFound {
		t.Fatalf("跨实例 DELETE 应 404: %d", w.Code)
	}
	// 实例 2 的列表不含实例 1 的行
	if w := getJSON(r, "/api/v1/instances/"+itoa(id2)+"/schedules", token); w.Code != http.StatusOK {
		t.Fatalf("GET schedules: %d", w.Code)
	} else if list, _ := decode(t, w.Body.Bytes())["schedules"].([]any); len(list) != 0 {
		t.Fatalf("实例 2 列表应为空: %s", w.Body.String())
	}
}

func TestInstanceDeleteCascadesSchedulesAndBackups(t *testing.T) {
	r, deps, token := newBackupEnv(t, 20) // 真实备份服务
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	writeSaveTree(t, dir)

	// 一条备份 + 一条 schedule
	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token, map[string]string{})
	jobID := decode(t, w.Body.Bytes())["job_id"].(string)
	waitJobState(t, deps, jobID, "done", 5*time.Second)
	w = getJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token)
	rec := decode(t, w.Body.Bytes())["backups"].([]any)[0].(map[string]any)
	full, err := deps.Backups.Get(id, int64(rec["id"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	backupFile := full.File

	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/schedules", token,
		map[string]any{"kind": "backup", "cron_expr": "0 3 * * *"})
	if w.Code != http.StatusOK {
		t.Fatalf("POST schedules: %d %s", w.Code, w.Body.String())
	}

	// 删除实例 → 级联清理 schedules 行、backups 行与文件
	if w := deleteJSON(r, "/api/v1/instances/"+itoa(id), token); w.Code != http.StatusOK {
		t.Fatalf("DELETE instance: %d %s", w.Code, w.Body.String())
	}
	if n := countRows(t, deps.DB, `SELECT COUNT(*) FROM schedules WHERE instance_id=?`, id); n != 0 {
		t.Fatalf("schedules 行应级联删除，剩 %d", n)
	}
	if n := countRows(t, deps.DB, `SELECT COUNT(*) FROM backups WHERE instance_id=?`, id); n != 0 {
		t.Fatalf("backups 行应级联删除，剩 %d", n)
	}
	if _, err := os.Stat(backupFile); !os.IsNotExist(err) {
		t.Fatalf("备份文件应级联删除: %v", err)
	}
}
