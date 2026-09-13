// 配置端点测试：GET schema+values+hint、PUT 校验/空值跳过/ini 回写+审计+RCON 热应用、
// 实例级 job 互斥交叉场景（update↔restore 双向、start/backup 在恢复期间 409）、viewer 403。
package api

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"palpanel/internal/backup"
	"palpanel/internal/instance"
	"palpanel/internal/job"
	"palpanel/internal/rcon"
	"palpanel/internal/scheduler"
	"palpanel/internal/supervisor"
)

// writeINI 在 gameDir 下写一份最小 PalWorldSettings.ini（含未管理键）。
func writeINI(t *testing.T, gameDir, body string) {
	t.Helper()
	p := filepath.Join(gameDir, "Pal", "Saved", "Config", "WindowsServer", "PalWorldSettings.ini")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const sampleINI = `[/Script/Pal.PalGameWorldSettings]
OptionSettings=(DayTimeSpeedRate=1.000000,ServerName="My Server",CustomUnknown=xyz)
`

func readINI(t *testing.T, gameDir string) string {
	t.Helper()
	p := filepath.Join(gameDir, "Pal", "Saved", "Config", "WindowsServer", "PalWorldSettings.ini")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- GET /instances/{id}/config ----

func TestConfigGetSchemaAndValues(t *testing.T) {
	r, _, token := setupAdmin(t)
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	writeINI(t, dir, sampleINI)

	w := getJSON(r, "/api/v1/instances/"+itoa(id)+"/config", token)
	if w.Code != http.StatusOK {
		t.Fatalf("GET config: %d %s", w.Code, w.Body.String())
	}
	m := decode(t, w.Body.Bytes())
	fields, _ := m["fields"].([]any)
	if len(fields) != 120 {
		t.Fatalf("schema 应为全量 120 键，得到 %d", len(fields))
	}
	f0 := fields[0].(map[string]any)
	for _, k := range []string{"key", "type", "default", "group", "help"} {
		if _, ok := f0[k]; !ok {
			t.Fatalf("字段 DTO 缺少 %s: %v", k, f0)
		}
	}
	values, _ := m["values"].(map[string]any)
	if values["DayTimeSpeedRate"] != "1.000000" || values["ServerName"] != "My Server" {
		t.Fatalf("values 应含 ini 当前值: %v", values)
	}
	if values["CustomUnknown"] != "xyz" {
		t.Fatalf("未管理键应原样返回: %v", values)
	}
	if m["world_option_hint"] != "" {
		t.Fatalf("无 WorldOption.sav 时提示应为空: %v", m["world_option_hint"])
	}
}

func TestConfigGetNoINI(t *testing.T) {
	r, _, token := setupAdmin(t)
	id := createInstance(t, r, token, t.TempDir())

	w := getJSON(r, "/api/v1/instances/"+itoa(id)+"/config", token)
	if w.Code != http.StatusOK {
		t.Fatalf("GET config: %d %s", w.Code, w.Body.String())
	}
	m := decode(t, w.Body.Bytes())
	if values, _ := m["values"].(map[string]any); len(values) != 0 {
		t.Fatalf("无 ini 时 values 应为空: %v", values)
	}
	if h, _ := m["world_option_hint"].(string); !strings.Contains(h, "将生成新配置文件") {
		t.Fatalf("无 ini 时提示应为 将生成新配置文件: %q", h)
	}
}

func TestConfigGetWorldOptionHint(t *testing.T) {
	r, _, token := setupAdmin(t)
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	writeINI(t, dir, sampleINI)
	// WorldOption.sav 比 ini 新：粗检测应给提示
	sav := filepath.Join(dir, "Pal", "Saved", "SaveGames", "0", "world1", "WorldOption.sav")
	if err := os.MkdirAll(filepath.Dir(sav), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sav, []byte("GVAS"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(sav, future, future); err != nil {
		t.Fatal(err)
	}

	w := getJSON(r, "/api/v1/instances/"+itoa(id)+"/config", token)
	m := decode(t, w.Body.Bytes())
	if h, _ := m["world_option_hint"].(string); h == "" {
		t.Fatalf("WorldOption.sav 更新时应给出提示: %s", w.Body.String())
	}
}

// ---- PUT /instances/{id}/config ----

func TestConfigPutValidation(t *testing.T) {
	r, _, token := setupAdmin(t)
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	writeINI(t, dir, sampleINI)

	// 非法值 → 400 带键名
	w := putJSON(r, "/api/v1/instances/"+itoa(id)+"/config", token,
		map[string]any{"values": map[string]string{"DayTimeSpeedRate": "abc"}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法值应 400: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "DayTimeSpeedRate") {
		t.Fatalf("400 应带键名: %s", w.Body.String())
	}
	// 未知键 → 400
	w = putJSON(r, "/api/v1/instances/"+itoa(id)+"/config", token,
		map[string]any{"values": map[string]string{"NotAKey": "1"}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "NotAKey") {
		t.Fatalf("未知键应 400 带键名: %d %s", w.Code, w.Body.String())
	}
	// 空 values → 400
	w = putJSON(r, "/api/v1/instances/"+itoa(id)+"/config", token, map[string]any{"values": map[string]string{}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "没有需要保存的配置") {
		t.Fatalf("空 values 应 400: %d %s", w.Code, w.Body.String())
	}
	// 全空串（视为未设置）→ 400
	w = putJSON(r, "/api/v1/instances/"+itoa(id)+"/config", token,
		map[string]any{"values": map[string]string{"ServerName": ""}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "没有需要保存的配置") {
		t.Fatalf("全空串应 400: %d %s", w.Code, w.Body.String())
	}
}

func TestConfigPutWritesINIAndAudit(t *testing.T) {
	r, deps, token := setupAdminDeps(t)
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	writeINI(t, dir, sampleINI)

	w := putJSON(r, "/api/v1/instances/"+itoa(id)+"/config", token,
		map[string]any{"values": map[string]string{
			"DayTimeSpeedRate": "2.0",
			"ServerName":       "", // 空串=未设置，跳过
			"ServerPassword":   "p,w\"q",
		}})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT config: %d %s", w.Code, w.Body.String())
	}
	m := decode(t, w.Body.Bytes())
	if m["ok"] != true {
		t.Fatalf("应返回 ok: %s", w.Body.String())
	}
	if h, _ := m["hint"].(string); !strings.Contains(h, "重启实例后完全生效") {
		t.Fatalf("应返回生效提示: %s", w.Body.String())
	}
	// ini 真实变化：覆盖值生效、空串保持原值、未管理键保留、转义往返
	content := readINI(t, dir)
	if !strings.Contains(content, "DayTimeSpeedRate=2.000000") {
		t.Fatalf("float 应归一 6 位小数: %s", content)
	}
	if !strings.Contains(content, "ServerName=My Server") {
		t.Fatalf("空串跳过应保留原值（quoteValue 按需加引号）: %s", content)
	}
	if !strings.Contains(content, `ServerPassword="p,w""q"`) || !strings.Contains(content, "CustomUnknown=xyz") {
		t.Fatalf("引号转义/未管理键应保留: %s", content)
	}
	// 审计落库：detail 为变更键列表（不含空串键）
	var detail string
	if err := deps.DB.QueryRow(`SELECT detail FROM audit_logs WHERE action='config.write' AND instance_id=?`, id).Scan(&detail); err != nil {
		t.Fatalf("审计未落库: %v", err)
	}
	if !strings.Contains(detail, "DayTimeSpeedRate") || !strings.Contains(detail, "ServerPassword") ||
		strings.Contains(detail, "ServerName") {
		t.Fatalf("审计 detail 应为变更键列表（不含空串键）: %q", detail)
	}
	// C1：API 级往返——保存后的 ini 必须带段头，紧接的 GET 不报错且与写入一致
	w2 := getJSON(r, "/api/v1/instances/"+itoa(id)+"/config", token)
	if w2.Code != http.StatusOK {
		t.Fatalf("PUT 后 GET config: %d %s", w2.Code, w2.Body.String())
	}
	values, _ := decode(t, w2.Body.Bytes())["values"].(map[string]any)
	want := map[string]string{
		"DayTimeSpeedRate": "2.000000",
		"ServerName":       "My Server",     // 空串跳过 → 保留原值
		"ServerPassword":   `p,w"q`,         // 转义往返
		"CustomUnknown":    "xyz",           // 未管理键保留
	}
	for k, exp := range want {
		if values[k] != exp {
			t.Fatalf("往返 values[%q] = %q, 期望 %q", k, values[k], exp)
		}
	}
}

// ---- fake RCON 服务器（Source RCON 最小实现：回显 id/type=0） ----

type fakeRCOServer struct {
	ln net.Listener

	mu   sync.Mutex
	cmds []string
}

func newFakeRCOServer(t *testing.T) *fakeRCOServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeRCOServer{ln: ln}
	go s.acceptLoop()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *fakeRCOServer) Addr() string { return s.ln.Addr().String() }

func (s *fakeRCOServer) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

func (s *fakeRCOServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serve(conn)
	}
}

// serve 逐帧读取：对每帧原 id 回 type=0 空响应，type=2（命令）记录 body。
func (s *fakeRCOServer) serve(conn net.Conn) {
	defer conn.Close()
	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		size := binary.LittleEndian.Uint32(hdr)
		body := make([]byte, size)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		id := binary.LittleEndian.Uint32(body[0:])
		typ := binary.LittleEndian.Uint32(body[4:])
		if typ == 2 {
			s.mu.Lock()
			s.cmds = append(s.cmds, string(body[8:size-2]))
			s.mu.Unlock()
		}
		// 响应帧：size=10（id4+type4+NUL2），type=0，空 body
		resp := make([]byte, 14)
		binary.LittleEndian.PutUint32(resp[0:], 10)
		binary.LittleEndian.PutUint32(resp[4:], id)
		binary.LittleEndian.PutUint32(resp[8:], 0)
		if _, err := conn.Write(resp); err != nil {
			return
		}
	}
}

func TestConfigPutRCONHotApply(t *testing.T) {
	rconSrv := newFakeRCOServer(t)
	r, deps, token := setupAdminCfg(t, func(d *Deps) {
		d.RCONFor = func(instance.Instance) (*rcon.Conn, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return rcon.Dial(ctx, rconSrv.Addr(), "any-pw")
		}
	})
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	writeINI(t, dir, sampleINI)

	// 实例运行中才热应用
	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/start", token, nil); w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	waitStatus(t, deps, id, supervisor.StateRunning, 3*time.Second)

	w := putJSON(r, "/api/v1/instances/"+itoa(id)+"/config", token,
		map[string]any{"values": map[string]string{"DayTimeSpeedRate": "2.0", "ServerName": ""}})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT config: %d %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(rconSrv.Commands()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	cmds := rconSrv.Commands()
	if len(cmds) != 1 || cmds[0] != "OptionSettings DayTimeSpeedRate 2.0" {
		t.Fatalf("RCON 应收到热应用命令（空串键跳过），得到 %v", cmds)
	}
}

func TestConfigPutRCONFailureSilent(t *testing.T) {
	r, _, token := setupAdminCfg(t, func(d *Deps) {
		d.RCONFor = func(instance.Instance) (*rcon.Conn, error) {
			return nil, errors.New("连接失败")
		}
	})
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	writeINI(t, dir, sampleINI)
	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/start", token, nil); w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}

	w := putJSON(r, "/api/v1/instances/"+itoa(id)+"/config", token,
		map[string]any{"values": map[string]string{"DayTimeSpeedRate": "2.0"}})
	if w.Code != http.StatusOK {
		t.Fatalf("RCON 失败不应阻断保存: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(readINI(t, dir), "DayTimeSpeedRate=2.000000") {
		t.Fatal("RCON 失败时 ini 仍应写入")
	}
}

func TestConfigViewerForbidden(t *testing.T) {
	r, deps, token := setupAdminDeps(t)
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)

	roleID := queryInt64(t, deps.DB, `SELECT id FROM roles WHERE name='viewer'`)
	w := postJSON(r, "/api/v1/users", token, map[string]any{
		"username": "obs", "password": "viewer-pass-1", "role_ids": []int64{roleID}})
	if w.Code != http.StatusOK {
		t.Fatalf("create viewer: %d %s", w.Code, w.Body.String())
	}
	viewer := loginToken(t, r, "obs", "viewer-pass-1")

	if w := getJSON(r, "/api/v1/instances/"+itoa(id)+"/config", viewer); w.Code != http.StatusForbidden {
		t.Fatalf("viewer 读配置应 403: %d", w.Code)
	}
	if w := putJSON(r, "/api/v1/instances/"+itoa(id)+"/config", viewer,
		map[string]any{"values": map[string]string{"ServerName": "x"}}); w.Code != http.StatusForbidden {
		t.Fatalf("viewer 写配置应 403: %d", w.Code)
	}
}

// ---- 实例级 job 互斥交叉场景 ----

// blockingRestoreSvc 阻塞的恢复服务：RunRestore 挂起直到 release，用于互斥测试。
type blockingRestoreSvc struct {
	fakeBackupSvc
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingRestoreSvc() *blockingRestoreSvc {
	return &blockingRestoreSvc{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingRestoreSvc) RunRestore(_ context.Context, _ instance.Instance, _ backup.BackupRecord) error {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return nil
}

// blockingInstaller 阻塞的安装服务：Install 挂起直到 release 或 ctx 取消。
type blockingInstaller struct {
	fakeInstaller
	release chan struct{}
}

func (b blockingInstaller) Install(ctx context.Context, gameDir string, report func(int, string), onLine func(string)) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.release:
		return nil
	}
}

func seedBackupRow(t *testing.T, deps Deps, id int64) int64 {
	t.Helper()
	bid, err := deps.Backups.Add(backup.BackupRecord{InstanceID: id, File: "fake.tar.gz",
		SizeBytes: 1, SHA256: "x", Type: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	return bid
}

// findJob 在任务列表中找指定实例与类型的任务（POST update 响应不回传 job_id）。
func findJob(t *testing.T, deps Deps, id int64, kind string) job.Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, j := range deps.Jobs.List() {
			if j.InstanceID == id && j.Kind == kind {
				return j
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("未找到实例 %d 的 %s 任务", id, kind)
	return job.Job{}
}

func TestRestoreBlockedWhileUpdateRunning(t *testing.T) {
	release := make(chan struct{})
	r, deps, token := setupAdminCfg(t, func(d *Deps) {
		d.Backups = backup.NewStore(d.DB)
		d.BackupSvc = &fakeBackupSvc{}
		d.Installer = blockingInstaller{release: release}
	})
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	seedBackupRow(t, deps, id)

	// 更新 job 挂起（running）
	w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/update", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("POST update: %d %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !deps.Jobs.RunningOfKind(id, "update") {
		time.Sleep(10 * time.Millisecond)
	}

	// 更新进行中 → 恢复 409
	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup/1/restore", token, nil); w.Code != http.StatusConflict {
		t.Fatalf("更新中恢复应 409: %d %s", w.Code, w.Body.String())
	}
	close(release)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && deps.Jobs.RunningOfKind(id, "update") {
		time.Sleep(10 * time.Millisecond)
	}
	// 更新结束后恢复放行
	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup/1/restore", token, nil); w.Code != http.StatusOK {
		t.Fatalf("更新结束后恢复应放行: %d %s", w.Code, w.Body.String())
	}
}

func TestRestoreRunningBlocksStartBackupUpdate(t *testing.T) {
	svc := newBlockingRestoreSvc()
	r, deps, token := setupAdminCfg(t, func(d *Deps) {
		d.Backups = backup.NewStore(d.DB)
		d.BackupSvc = svc
		d.Installer = fakeInstaller{}
	})
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
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

	// start → 409
	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/start", token, nil); w.Code != http.StatusConflict {
		t.Fatalf("恢复中启动应 409: %d %s", w.Code, w.Body.String())
	}
	// backup → 409
	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token, map[string]string{}); w.Code != http.StatusConflict {
		t.Fatalf("恢复中备份应 409: %d %s", w.Code, w.Body.String())
	}
	// update：job 层中止（failed，且不执行安装）
	w = postJSON(r, "/api/v1/instances/"+itoa(id)+"/update", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("POST update: %d %s", w.Code, w.Body.String())
	}
	updateJob := findJob(t, deps, id, "update")
	waitJobState(t, deps, updateJob.ID, "failed", 5*time.Second)
	if !strings.Contains(updateJob.Error, "恢复") {
		t.Fatalf("update 应因恢复进行中被中止: %+v", updateJob)
	}

	close(svc.release)
	waitJobState(t, deps, restoreJob, "done", 5*time.Second)
	// 恢复结束后全部放行
	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/start", token, nil); w.Code != http.StatusOK {
		t.Fatalf("恢复结束后启动应放行: %d %s", w.Code, w.Body.String())
	}
	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token, map[string]string{}); w.Code != http.StatusOK {
		t.Fatalf("恢复结束后备份应放行: %d %s", w.Code, w.Body.String())
	}
}

// ---- RunScheduled 分派（调度器执行体） ----

func TestRunScheduledDispatch(t *testing.T) {
	svc := newBlockingRestoreSvc()
	r, deps, token := setupAdminCfg(t, func(d *Deps) {
		d.Backups = backup.NewStore(d.DB)
		d.BackupSvc = svc
	})
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)
	bid := seedBackupRow(t, deps, id)

	// 恢复 job 挂起
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

	// 定时重启/定时备份在恢复期间被互斥跳过（报错由调度器记日志）
	if err := deps.RunScheduled(scheduler.SchedulesRow{InstanceID: id, Kind: "restart"}); err == nil {
		t.Fatal("恢复中定时重启应报错跳过")
	}
	if err := deps.RunScheduled(scheduler.SchedulesRow{InstanceID: id, Kind: "backup"}); err == nil {
		t.Fatal("恢复中定时备份应报错跳过")
	}
	close(svc.release)
	waitJobState(t, deps, restoreJob, "done", 5*time.Second)

	// 定时重启 → 停止（幂等容忍）后拉起
	if err := deps.RunScheduled(scheduler.SchedulesRow{InstanceID: id, Kind: "restart"}); err != nil {
		t.Fatalf("restart 分派失败: %v", err)
	}
	waitStatus(t, deps, id, supervisor.StateRunning, 3*time.Second)

	// 未知类型报错
	if err := deps.RunScheduled(scheduler.SchedulesRow{InstanceID: id, Kind: "nope"}); err == nil {
		t.Fatal("未知任务类型应报错")
	}
}

// ---- 备份互斥矩阵闭合（fix round 1）：backup↔restore↔update 两两互斥 ----

// blockingBackupSvc 阻塞的备份服务：RunBackup 挂起直到 release，模拟打包中。
type blockingBackupSvc struct {
	fakeBackupSvc
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingBackupSvc() *blockingBackupSvc {
	return &blockingBackupSvc{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingBackupSvc) RunBackup(_ context.Context, _ instance.Instance, typ, _ string) (backup.BackupRecord, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return backup.BackupRecord{Type: typ}, nil
}

func TestBackupBlockedWhileUpdateRunning(t *testing.T) {
	release := make(chan struct{})
	r, deps, token := setupAdminCfg(t, func(d *Deps) {
		d.Backups = backup.NewStore(d.DB)
		d.BackupSvc = &fakeBackupSvc{}
		d.Installer = blockingInstaller{release: release}
	})
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)

	// 更新 job 挂起
	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/update", token, nil); w.Code != http.StatusOK {
		t.Fatalf("POST update: %d %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !deps.Jobs.RunningOfKind(id, "update") {
		time.Sleep(10 * time.Millisecond)
	}
	// 更新进行中 → 手动备份 409
	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token, map[string]string{}); w.Code != http.StatusConflict {
		t.Fatalf("更新中手动备份应 409: %d %s", w.Code, w.Body.String())
	}
	close(release)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && deps.Jobs.RunningOfKind(id, "update") {
		time.Sleep(10 * time.Millisecond)
	}
	// 更新结束后放行
	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/backup", token, map[string]string{}); w.Code != http.StatusOK {
		t.Fatalf("更新结束后备份应放行: %d %s", w.Code, w.Body.String())
	}
}

func TestUpdateJobAbortsWhileBackupRunning(t *testing.T) {
	release := make(chan struct{})
	r, deps, token := setupAdminCfg(t, func(d *Deps) {
		d.Backups = backup.NewStore(d.DB)
		d.BackupSvc = &fakeBackupSvc{}
		d.Installer = fakeInstaller{installFn: func(_ context.Context, _ string, _ func(int, string), _ func(string)) error {
			return nil
		}}
	})
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)

	// 起一个挂起的手动备份 job（模拟打包中）
	if _, err := deps.Jobs.Start("backup", id, func(ctx context.Context, report func(int, string)) error {
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// update → job 层中止（failed，不执行安装/备份）
	if w := postJSON(r, "/api/v1/instances/"+itoa(id)+"/update", token, nil); w.Code != http.StatusOK {
		t.Fatalf("POST update: %d %s", w.Code, w.Body.String())
	}
	updateJob := findJob(t, deps, id, "update")
	waitJobState(t, deps, updateJob.ID, "failed", 5*time.Second)
	if !strings.Contains(updateJob.Error, "备份") {
		t.Fatalf("update 应因备份进行中被中止: %+v", updateJob)
	}
	close(release)
}

func TestScheduledBackupGoesThroughJobs(t *testing.T) {
	// ③ 定时备份与手动备份并发去重（ErrDuplicate → 跳过不算失败）
	release := make(chan struct{})
	fb := &fakeBackupSvc{}
	r, deps, token := setupAdminCfg(t, func(d *Deps) {
		d.Backups = backup.NewStore(d.DB)
		d.BackupSvc = fb
	})
	dir := t.TempDir()
	id := createInstance(t, r, token, dir)

	if _, err := deps.Jobs.Start("backup", id, func(ctx context.Context, report func(int, string)) error {
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 手动备份 job 在跑 → 定时备份防重跳过，不执行 RunBackup
	if err := deps.RunScheduled(scheduler.SchedulesRow{InstanceID: id, Kind: "backup"}); err != nil {
		t.Fatalf("定时备份防重应返回 nil: %v", err)
	}
	if got := fb.Calls(); len(got) != 0 {
		t.Fatalf("防重跳过时不应执行备份: %v", got)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && deps.Jobs.RunningOfKind(id, "backup") {
		time.Sleep(10 * time.Millisecond)
	}

	// ④ 定时备份走 job 体系后，restore 能经 RunningOfKind 感知 → 409
	bid := seedBackupRow(t, deps, id)
	svc := newBlockingBackupSvc()
	deps.BackupSvc = svc
	if err := deps.RunScheduled(scheduler.SchedulesRow{InstanceID: id, Kind: "backup"}); err != nil {
		t.Fatalf("定时备份分派: %v", err)
	}
	select {
	case <-svc.started:
	case <-time.After(3 * time.Second):
		t.Fatal("定时备份 job 未开始")
	}
	if w := postJSON(r, fmt.Sprintf("/api/v1/instances/%d/backup/%d/restore", id, bid), token, nil); w.Code != http.StatusConflict {
		t.Fatalf("定时备份打包中恢复应 409: %d %s", w.Code, w.Body.String())
	}
	close(svc.release)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && deps.Jobs.RunningOfKind(id, "backup") {
		time.Sleep(10 * time.Millisecond)
	}
}
