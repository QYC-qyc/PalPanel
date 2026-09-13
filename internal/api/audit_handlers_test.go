package api

import (
	"net/http"
	"strings"
	"testing"

	"palpanel/internal/auth"
)

// TestAuditList：审计查询 API——200、含 auth.setup、筛选可用、无权限 403。
func TestAuditList(t *testing.T) {
	r, database, admin := setupAdmin(t) // setup+login 已产生 auth.setup / auth.login 审计

	// 1) 管理员可查，含 auth.setup
	w := getJSON(r, "/api/v1/audit-logs?page=1&page_size=10", admin)
	if w.Code != http.StatusOK {
		t.Fatalf("audit list: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "auth.setup") {
		t.Fatal("audit should contain auth.setup")
	}

	// 2) 分页边界：page<1 修正为 1，page_size>100 截为 20
	w = getJSON(r, "/api/v1/audit-logs?page=0&page_size=500", admin)
	if w.Code != http.StatusOK {
		t.Fatalf("page bounds: %d %s", w.Code, w.Body.String())
	}
	if got := decode(t, w.Body.Bytes())["page_size"].(float64); got != 20 {
		t.Fatalf("page_size >100 should clamp to 20, got %v", got)
	}

	// 3) user_id / instance_id / action 筛选可用（不报错即可）
	if w = getJSON(r, "/api/v1/audit-logs?instance_id=1", admin); w.Code != http.StatusOK {
		t.Fatalf("instance_id filter: %d %s", w.Code, w.Body.String())
	}
	if w = getJSON(r, "/api/v1/audit-logs?user_id=1&action=auth.", admin); w.Code != http.StatusOK {
		t.Fatalf("user_id/action filter: %d %s", w.Code, w.Body.String())
	}

	// 4) 无 audit.read 权限的 viewer 用户 403。
	// 内置 viewer 角色默认含 audit.read，先微调其权限移除（产品允许微调内置 viewer），
	// 再建 viewer 用户登录访问，断言 403。
	viewerRole := queryInt64(t, database, `SELECT id FROM roles WHERE name='viewer'`)
	// 取 viewer 当前权限码，剔除 audit.read 后微调回去
	rows, err := database.Query(`SELECT code FROM role_permissions WHERE role_id=?`, viewerRole)
	if err != nil {
		t.Fatal(err)
	}
	var perms []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			t.Fatal(err)
		}
		if code != auth.PAuditRead {
			perms = append(perms, code)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if w = putJSON(r, "/api/v1/roles/"+itoa(viewerRole)+"/permissions", admin,
		map[string]any{"permissions": perms}); w.Code != http.StatusOK {
		t.Fatalf("strip viewer perms: %d %s", w.Code, w.Body.String())
	}
	postJSON(r, "/api/v1/users", admin, map[string]any{"username": "vv", "password": "good-pass-9"})
	viewerID := queryInt64(t, database, `SELECT id FROM users WHERE username='vv'`)
	putJSON(r, "/api/v1/users/"+itoa(viewerID)+"/roles", admin,
		map[string]any{"role_ids": []int64{viewerRole}})
	vvToken := loginToken(t, r, "vv", "good-pass-9")
	w = getJSON(r, "/api/v1/audit-logs", vvToken)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer without audit.read want 403 got %d %s", w.Code, w.Body.String())
	}
}
