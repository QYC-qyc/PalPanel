package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"palpanel/internal/backup"
	"palpanel/internal/instance"
	"palpanel/internal/supervisor"
)

// ---- 测试环境：真实 backup.Service（真实 Store + 临时目录真实文件） ----

// newBackupEnv 构造带真实备份服务的路由：keep 为滚动保留条数；
// stopper 经 Sup.Stop 代走并容忍未运行。
func newBackupEnv(t *testing.T, keep int) (*gin.Engine, Deps, string) {
	t.Helper()
	r, deps, token := setupAdminCfg(t, func(d *Deps) {
		store := backup.NewStore(d.DB)
		d.Backups = store
		d.BackupSvc = backup.NewService(store, func(id int64) error {
			err := d.Sup.Stop(id)
			if errors.Is(err, supervisor.ErrNotRunning) {
				return nil
			}
			return err
		}, keep)
	})
	return r, deps, token
}

// writeSaveTree 在 gameDir 下造一棵小 saves 树，返回 saves 目录路径。
func writeSaveTree(t *testing.T, gameDir string) string {
	t.Helper()
	saves := filepath.Join(gameDir, "Pal", "Saved", "SaveGames", "0")
	if err := os.MkdirAll(filepath.Join(saves, "Players"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saves, "Level.sav"), []byte("LEVEL-ORIG-存档"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saves, "Players", "x.sav"), []byte("player-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return saves
}

// backupIDFromList 从列表响应取第一条备份的 id。
func backupIDFromList(t *testing.T, body []byte) int64 {
	t.Helper()
	m := decode(t, body)
	list, _ := m["backups"].([]any)
	if len(list) == 0 {
		t.Fatalf("备份列表为空: %s", body)
	}
	return int64(list[0].(map[string]any)["id"].(float64))
}

func TestBackupListEmpty(t *testing.T) {
	r, _, token := newBackupEnv(t, 20)
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)

	w := getJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token)
	if w.Code != http.StatusOK {
		t.Fatalf("GET backup: %d %s", w.Code, w.Body.String())
	}
	list, _ := decode(t, w.Body.Bytes())["backups"].([]any)
	if len(list) != 0 {
		t.Fatalf("期望空列表，得到 %v", list)
	}
}

func TestBackupCreateManual(t *testing.T) {
	r, deps, token := newBackupEnv(t, 20)
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	writeSaveTree(t, dir)

	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token, map[string]string{"note": "手动备份"})
	if w.Code != http.StatusOK {
		t.Fatalf("POST backup: %d %s", w.Code, w.Body.String())
	}
	jobID, _ := decode(t, w.Body.Bytes())["job_id"].(string)
	if jobID == "" {
		t.Fatalf("响应缺少 job_id: %s", w.Body.String())
	}
	waitJobState(t, deps, jobID, "done", 5*time.Second)

	w = getJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token)
	m := decode(t, w.Body.Bytes())
	list, _ := m["backups"].([]any)
	if len(list) != 1 {
		t.Fatalf("期望 1 条备份，得到 %d: %s", len(list), w.Body.String())
	}
	rec := list[0].(map[string]any)
	if rec["type"] != "manual" || rec["note"] != "手动备份" {
		t.Fatalf("type/note 不符: %v", rec)
	}
	if rec["size_bytes"].(float64) <= 0 {
		t.Fatalf("size 应大于 0: %v", rec)
	}
	if rec["sha256"] == "" {
		t.Fatalf("sha256 不应为空: %v", rec)
	}
	// 备份文件真实存在（列表 file 为文件名，完整路径经库行核对）
	full, err := deps.Backups.Get(id, int64(rec["id"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(full.File); err != nil {
		t.Fatalf("备份文件不存在: %v", err)
	}
}

func TestBackupCreateSavesMissing(t *testing.T) {
	r, deps, token := newBackupEnv(t, 20)
	id := createInstance(t, r, token, t.TempDir()) // 不造 saves 树

	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token, map[string]string{})
	if w.Code != http.StatusOK {
		t.Fatalf("POST backup: %d %s", w.Code, w.Body.String())
	}
	jobID := decode(t, w.Body.Bytes())["job_id"].(string)
	waitJobState(t, deps, jobID, "failed", 5*time.Second)
	j, _ := deps.Jobs.Get(jobID)
	if !strings.Contains(j.Error, "存档目录不存在") {
		t.Fatalf("错误信息应含 存档目录不存在: %q", j.Error)
	}
}

func TestBackupRestoreRestoresContent(t *testing.T) {
	r, deps, token := newBackupEnv(t, 20)
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	saves := writeSaveTree(t, dir)

	// 1. 先建一份好备份
	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token, map[string]string{"note": "好备份"})
	jobID := decode(t, w.Body.Bytes())["job_id"].(string)
	waitJobState(t, deps, jobID, "done", 5*time.Second)

	w = getJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token)
	bid := backupIDFromList(t, w.Body.Bytes())

	// 2. 改坏 saves
	if err := os.WriteFile(filepath.Join(saves, "Level.sav"), []byte("CORRUPTED"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. 恢复
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup/"+itoa(bid)+"/restore", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("POST restore: %d %s", w.Code, w.Body.String())
	}
	jobID = decode(t, w.Body.Bytes())["job_id"].(string)
	waitJobState(t, deps, jobID, "done", 5*time.Second)

	// 内容逐字节还原
	b, err := os.ReadFile(filepath.Join(saves, "Level.sav"))
	if err != nil || string(b) != "LEVEL-ORIG-存档" {
		t.Fatalf("Level.sav 未还原: %q err=%v", string(b), err)
	}
	// 恢复前自动备份存在（type=pre-restore）
	w = getJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token)
	list, _ := decode(t, w.Body.Bytes())["backups"].([]any)
	foundPre := false
	for _, it := range list {
		if it.(map[string]any)["type"] == "pre-restore" {
			foundPre = true
		}
	}
	if !foundPre {
		t.Fatalf("缺少 pre-restore 备份: %s", w.Body.String())
	}
}

func TestBackupRestoreBadPackage(t *testing.T) {
	r, deps, token := newBackupEnv(t, 20)
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	saves := writeSaveTree(t, dir)

	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token, map[string]string{})
	jobID := decode(t, w.Body.Bytes())["job_id"].(string)
	waitJobState(t, deps, jobID, "done", 5*time.Second)
	w = getJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token)
	bid := backupIDFromList(t, w.Body.Bytes())

	// 备份包损坏后拒绝恢复（恢复前 Verify 失败 → job failed，saves 不被动）
	pkg, _ := deps.Backups.Get(id, bid)
	if err := os.WriteFile(pkg.File, []byte("not a tar"), 0o644); err != nil {
		t.Fatal(err)
	}
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup/"+itoa(bid)+"/restore", token, nil)
	jobID = decode(t, w.Body.Bytes())["job_id"].(string)
	waitJobState(t, deps, jobID, "failed", 5*time.Second)
	b, err := os.ReadFile(filepath.Join(saves, "Level.sav"))
	if err != nil || string(b) != "LEVEL-ORIG-存档" {
		t.Fatalf("损坏包恢复失败时 saves 不应被改动: %q %v", string(b), err)
	}
}

func TestBackupDeleteRemovesRowAndFile(t *testing.T) {
	r, deps, token := newBackupEnv(t, 20)
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	writeSaveTree(t, dir)

	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token, map[string]string{})
	jobID := decode(t, w.Body.Bytes())["job_id"].(string)
	waitJobState(t, deps, jobID, "done", 5*time.Second)
	w = getJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token)
	bid := backupIDFromList(t, w.Body.Bytes())
	rec, err := deps.Backups.Get(id, bid)
	if err != nil {
		t.Fatal(err)
	}

	w = deleteJSON(r, "/api/v1/instances/"+itoa(id)+"/backup/"+itoa(bid), token)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE backup: %d %s", w.Code, w.Body.String())
	}
	if n := countRows(t, deps.DB, `SELECT COUNT(*) FROM backups WHERE instance_id=?`, id); n != 0 {
		t.Fatalf("库行应已删除，剩 %d", n)
	}
	if _, err := os.Stat(rec.File); !os.IsNotExist(err) {
		t.Fatalf("备份文件应已删除: %v", err)
	}
	// 再删 → 404
	w = deleteJSON(r, "/api/v1/instances/"+itoa(id)+"/backup/"+itoa(bid), token)
	if w.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404: %d", w.Code)
	}
}

func TestBackupKeepCountCleanup(t *testing.T) {
	r, deps, token := newBackupEnv(t, 1) // 只保留 1 份
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	writeSaveTree(t, dir)

	var firstFile string
	for i := 0; i < 2; i++ {
		w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token, map[string]string{})
		jobID := decode(t, w.Body.Bytes())["job_id"].(string)
		waitJobState(t, deps, jobID, "done", 5*time.Second)
		w = getJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token)
		list, _ := decode(t, w.Body.Bytes())["backups"].([]any)
		if len(list) != 1 {
			t.Fatalf("keep=1 应只保留 1 条，得到 %d", len(list))
		}
		if i == 0 {
			full, err := deps.Backups.Get(id, int64(list[0].(map[string]any)["id"].(float64)))
			if err != nil {
				t.Fatal(err)
			}
			firstFile = full.File
		}
	}
	if _, err := os.Stat(firstFile); !os.IsNotExist(err) {
		t.Fatalf("被滚出的备份文件应已删除: %v", err)
	}
}

func TestBackupViewerForbidden(t *testing.T) {
	r, deps, token := newBackupEnv(t, 20)
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	writeSaveTree(t, dir)

	roleID := queryInt64(t, deps.DB, `SELECT id FROM roles WHERE name='viewer'`)
	w := postJSON(r, "/api/v1/users", token, map[string]any{
		"username": "obs", "password": "viewer-pass-1", "role_ids": []int64{roleID}})
	if w.Code != http.StatusOK {
		t.Fatalf("create viewer: %d %s", w.Code, w.Body.String())
	}
	viewer := loginToken(t, r, "obs", "viewer-pass-1")

	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", viewer, nil); w.Code != http.StatusForbidden {
		t.Fatalf("viewer 创建备份应 403: %d", w.Code)
	}
	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup/1/restore", viewer, nil); w.Code != http.StatusForbidden {
		t.Fatalf("viewer 恢复应 403: %d", w.Code)
	}
	if w := deleteJSON(r, "/api/v1/instances/"+itoa(id)+"/backup/1", viewer); w.Code != http.StatusForbidden {
		t.Fatalf("viewer 删除备份应 403: %d", w.Code)
	}
	if w := getJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", viewer); w.Code != http.StatusForbidden {
		t.Fatalf("viewer 无实例授权读取备份应 403: %d", w.Code)
	}
}

// ---- fake BackupRunner（update job 的 pre-update 备份断言） ----

type fakeBackupSvc struct {
	mu    sync.Mutex
	calls []string
	err   error // RunBackup 返回的错误
}

func (f *fakeBackupSvc) RunBackup(_ context.Context, _ instance.Instance, typ, _ string) (backup.BackupRecord, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "backup:"+typ)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return backup.BackupRecord{}, err
	}
	return backup.BackupRecord{ID: 1, InstanceID: 1, Type: typ}, nil
}

func (f *fakeBackupSvc) RunRestore(_ context.Context, _ instance.Instance, _ backup.BackupRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "restore")
	return nil
}

func (f *fakeBackupSvc) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func TestUpdateJobRunsPreUpdateBackup(t *testing.T) {
	r, deps, token := setupAdminCfg(t, func(d *Deps) {
		fb := &fakeBackupSvc{}
		d.BackupSvc = fb
		d.Installer = fakeInstaller{installFn: func(_ context.Context, _ string, _ func(int, string), _ func(string)) error {
			fb.mu.Lock()
			fb.calls = append(fb.calls, "install")
			fb.mu.Unlock()
			return nil
		}}
	})
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	writeSaveTree(t, dir)

	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/update", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("POST update: %d %s", w.Code, w.Body.String())
	}
	// job 通过 fake 记录调用，等 backup+install 都发生
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := deps.BackupSvc.(*fakeBackupSvc).Calls(); len(got) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := deps.BackupSvc.(*fakeBackupSvc).Calls()
	if len(got) != 2 || got[0] != "backup:pre-update" || got[1] != "install" {
		t.Fatalf("更新链应为 [backup:pre-update install]，得到 %v", got)
	}
}

func TestUpdateJobAbortsWhenBackupFails(t *testing.T) {
	r, deps, token := setupAdminCfg(t, func(d *Deps) {
		fb := &fakeBackupSvc{err: errors.New("磁盘已满")}
		d.BackupSvc = fb
		d.Installer = fakeInstaller{installFn: func(_ context.Context, _ string, _ func(int, string), _ func(string)) error {
			fb.mu.Lock()
			fb.calls = append(fb.calls, "install")
			fb.mu.Unlock()
			return nil
		}}
	})
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)

	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/update", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("POST update: %d %s", w.Code, w.Body.String())
	}
	// 等 job 结束（failed 或 done 皆说明已收尾）
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := deps.BackupSvc.(*fakeBackupSvc).Calls()
		if len(got) >= 1 {
			// RunBackup 已被调，稍等 job 收尾
			time.Sleep(50 * time.Millisecond)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	got := deps.BackupSvc.(*fakeBackupSvc).Calls()
	for _, c := range got {
		if c == "install" {
			t.Fatalf("备份失败后不应执行安装: %v", got)
		}
	}
	if len(got) != 1 || got[0] != "backup:pre-update" {
		t.Fatalf("应只调一次 pre-update 备份: %v", got)
	}
}
