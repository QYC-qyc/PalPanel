package api

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"palpanel/internal/backup"
)

// 创建 viewer 低权用户 + 一个实例，验证 grant 前后可见性与密码脱敏。
func TestInstanceVisibilityAndSecret(t *testing.T) {
	r, database, admin := setupAdmin(t)

	// 建实例（admin 有 instance.manage）
	body := map[string]any{
		"name": "s1", "game_dir": filepath.Join(t.TempDir(), "s1"),
		"game_port": 8211, "query_port": 27015, "admin_password": "secret-pw-1"}
	w := postJSON(r, "/api/v1/instances", admin, body)
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	// 响应不得包含明文密码
	if got := w.Body.String(); strings.Contains(got, "secret-pw-1") {
		t.Fatal("response leaks admin password")
	}
	iid := int64(decode(t, w.Body.Bytes())["id"].(float64))

	// rest_port 未传时与 game_port 相同（admin 走列表验证，detail 需实例 grant）
	w = getJSON(r, "/api/v1/instances", admin)
	if w.Code != http.StatusOK {
		t.Fatalf("list instances: %d %s", w.Code, w.Body.String())
	}
	insts := decode(t, w.Body.Bytes())["instances"].([]any)
	if len(insts) != 1 {
		t.Fatalf("want 1 instance got %d", len(insts))
	}
	inst := insts[0].(map[string]any)
	if inst["rest_port"].(float64) != 8211 {
		t.Fatalf("rest_port should default to game_port, got %v", inst["rest_port"])
	}

	// 端口冲突 409
	body2 := map[string]any{"name": "s2", "game_dir": filepath.Join(t.TempDir(), "s2"),
		"game_port": 8211, "query_port": 27016, "admin_password": "secret-pw-2"}
	if w = postJSON(r, "/api/v1/instances", admin, body2); w.Code != http.StatusConflict {
		t.Fatalf("conflict want 409 got %d", w.Code)
	}

	// 低权用户（viewer）无 grant → 列表看不到
	postJSON(r, "/api/v1/users", admin, map[string]any{"username": "vv", "password": "good-pass-9"})
	viewerID := queryInt64(t, database, `SELECT id FROM users WHERE username='vv'`)
	viewerRole := queryInt64(t, database, `SELECT id FROM roles WHERE name='viewer'`)
	putJSON(r, "/api/v1/users/"+itoa(viewerID)+"/roles", admin,
		map[string]any{"role_ids": []int64{viewerRole}})
	vvToken := loginToken(t, r, "vv", "good-pass-9")
	w = getJSON(r, "/api/v1/instances", vvToken)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "s1") {
		t.Fatalf("viewer without grant should see nothing: %d %s", w.Code, w.Body.String())
	}
	// 授予 grant（ReplaceUserGrants 会 bump role_version）后重新登录，可见但 PATCH 仍 403
	putJSON(r, "/api/v1/users/"+itoa(viewerID)+"/grants", admin,
		map[string]any{"instance_ids": []int64{iid}})
	vvToken = loginToken(t, r, "vv", "good-pass-9")
	w = getJSON(r, "/api/v1/instances", vvToken)
	if !strings.Contains(w.Body.String(), "s1") {
		t.Fatalf("viewer with grant should see s1: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret-pw-1") {
		t.Fatal("list leaks admin password")
	}
	w = patchJSON(r, "/api/v1/instances/"+itoa(iid), vvToken, map[string]any{"name": "hacked"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer patch want 403 got %d", w.Code)
	}

	// admin 无需实例 grant 即可详情与删除（instance.manage 隐式全实例授权，回归）
	w = getJSON(r, "/api/v1/instances/"+itoa(iid), admin)
	if w.Code != http.StatusOK {
		t.Fatalf("admin get detail want 200 got %d %s", w.Code, w.Body.String())
	}
	w = deleteJSON(r, "/api/v1/instances/"+itoa(iid), admin)
	if w.Code != http.StatusOK {
		t.Fatalf("admin delete want 200 got %d %s", w.Code, w.Body.String())
	}
}

// TestInstancePasswordMinLength：实例密码与用户密码一致，至少 8 位（创建与 PATCH 均校验）。
func TestInstancePasswordMinLength(t *testing.T) {
	r, _, admin := setupAdmin(t)

	// 创建：6 位密码 → 400
	body := map[string]any{"name": "s1", "game_dir": filepath.Join(t.TempDir(), "s1"),
		"game_port": 8211, "query_port": 27015, "admin_password": "abc123"}
	if w := postJSON(r, "/api/v1/instances", admin, body); w.Code != http.StatusBadRequest {
		t.Fatalf("6-char password want 400 got %d %s", w.Code, w.Body.String())
	}

	// 建 8 位密码实例后 PATCH 成 6 位 → 400
	body["admin_password"] = "long-enough-1"
	w := postJSON(r, "/api/v1/instances", admin, body)
	if w.Code != http.StatusOK {
		t.Fatalf("create with 8+ chars: %d %s", w.Code, w.Body.String())
	}
	iid := int64(decode(t, w.Body.Bytes())["id"].(float64))
	if w := patchJSON(r, "/api/v1/instances/"+itoa(iid), admin,
		map[string]any{"admin_password": "abc123"}); w.Code != http.StatusBadRequest {
		t.Fatalf("patch 6-char password want 400 got %d %s", w.Code, w.Body.String())
	}
}

// TestInstanceDeleteCleansGrants：删除实例后 instance_grants 无该实例残留。
func TestInstanceDeleteCleansGrants(t *testing.T) {
	r, database, admin := setupAdmin(t)

	body := map[string]any{"name": "s1", "game_dir": filepath.Join(t.TempDir(), "s1"),
		"game_port": 8211, "query_port": 27015, "admin_password": "secret-pw-1"}
	w := postJSON(r, "/api/v1/instances", admin, body)
	if w.Code != http.StatusOK {
		t.Fatalf("create instance: %d %s", w.Code, w.Body.String())
	}
	iid := int64(decode(t, w.Body.Bytes())["id"].(float64))

	// viewer 用户 + 实例 grant
	w = postJSON(r, "/api/v1/users", admin, map[string]any{
		"username": "vv", "password": "good-pass-7"})
	if w.Code != http.StatusOK {
		t.Fatalf("create user: %d %s", w.Code, w.Body.String())
	}
	vid := queryInt64(t, database, `SELECT id FROM users WHERE username='vv'`)
	if w := putJSON(r, "/api/v1/users/"+itoa(vid)+"/grants", admin,
		map[string]any{"instance_ids": []int64{iid}}); w.Code != http.StatusOK {
		t.Fatalf("grants: %d %s", w.Code, w.Body.String())
	}

	if w := deleteJSON(r, "/api/v1/instances/"+itoa(iid), admin); w.Code != http.StatusOK {
		t.Fatalf("delete instance: %d %s", w.Code, w.Body.String())
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM instance_grants WHERE instance_id=?`, iid); n != 0 {
		t.Fatalf("instance_grants residual rows: %d", n)
	}
}

// I2：恢复 job 运行中删除实例应 409，且实例库行未删（避免孤儿行/竞态写）。
func TestInstanceDeleteBlockedWhileRestoreRunning(t *testing.T) {
	svc := newBlockingRestoreSvc()
	r, deps, token := setupAdminCfg(t, func(d *Deps) {
		d.Backups = backup.NewStore(d.DB)
		d.BackupSvc = svc
	})
	id := createInstance(t, r, token, t.TempDir())
	bid := seedBackupRow(t, deps, id)

	// 恢复 job 挂起（running）
	w := postJSON(r, fmt.Sprintf("/api/v1/instances/%d/backup/%d/restore", id, bid), token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("POST restore: %d %s", w.Code, w.Body.String())
	}
	restoreJob := decode(t, w.Body.Bytes())["job_id"].(string)
	select {
	case <-svc.started:
	case <-time.After(3 * time.Second):
		t.Fatal("恢复 job 未开始")
	}

	w = deleteJSON(r, "/api/v1/instances/"+itoa(id), token)
	if w.Code != http.StatusConflict {
		t.Fatalf("恢复中删除实例应 409: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "进行中") {
		t.Fatalf("409 应说明存在进行中的任务: %s", w.Body.String())
	}
	if n := countRows(t, deps.DB, `SELECT COUNT(*) FROM instances WHERE id=?`, id); n != 1 {
		t.Fatalf("实例库行不应被删除，剩 %d", n)
	}

	// 恢复结束后删除放行
	close(svc.release)
	waitJobState(t, deps, restoreJob, "done", 5*time.Second)
	if w := deleteJSON(r, "/api/v1/instances/"+itoa(id), token); w.Code != http.StatusOK {
		t.Fatalf("恢复结束后删除应放行: %d %s", w.Code, w.Body.String())
	}
}

// I2：备份 job 运行中删除实例同样应 409。
func TestInstanceDeleteBlockedWhileBackupRunning(t *testing.T) {
	svc := newBlockingBackupSvc()
	r, deps, token := setupAdminCfg(t, func(d *Deps) {
		d.Backups = backup.NewStore(d.DB)
		d.BackupSvc = svc
	})
	id := createInstance(t, r, token, t.TempDir())

	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token, map[string]string{}); w.Code != http.StatusOK {
		t.Fatalf("POST backup: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-svc.started:
	case <-time.After(3 * time.Second):
		t.Fatal("备份 job 未开始")
	}

	if w := deleteJSON(r, "/api/v1/instances/"+itoa(id), token); w.Code != http.StatusConflict {
		t.Fatalf("备份中删除实例应 409: %d %s", w.Code, w.Body.String())
	}
	if n := countRows(t, deps.DB, `SELECT COUNT(*) FROM instances WHERE id=?`, id); n != 1 {
		t.Fatalf("实例库行不应被删除，剩 %d", n)
	}
	close(svc.release)
}
