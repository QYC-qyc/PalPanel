package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"palpanel/internal/steamcmd"
)

// newFakeRunner 返回 Command 被注入为「输出 fixture 内容、并记录 args」的 Service。
// Runner.Dir 预置 SteamCMD 可执行入口使 Ensure 短路（Downloader 若被调用即测试失败）。
// fixture 经临时文件输出（Windows `cmd /c type` / Linux `sh -c cat`）：
// 直接 `cmd /c echo "..."` 会因 Go 参数转义引入反斜杠，破坏 buildid 正则匹配。
func newFakeRunner(t *testing.T, output string) (*Service, *[]string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, steamcmd.ExeName()), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(outFile, []byte(output+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls []string
	r := steamcmd.NewRunner(dir)
	r.Command = func(ctx context.Context, args []string) *exec.Cmd {
		calls = append(calls, strings.Join(args, " "))
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/c", "type", outFile)
		}
		return exec.Command("sh", "-c", "cat '"+outFile+"'")
	}
	dl := func(ctx context.Context, url, dest string) error {
		t.Fatal("SteamCMD 已存在时不应触发下载")
		return nil
	}
	return New(r, dl), &calls
}

func writeManifest(t *testing.T, gameDir, buildID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(gameDir, "steamapps"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`"AppState" { "buildid" "%s" }`, buildID)
	if err := os.WriteFile(filepath.Join(gameDir, "steamapps", "appmanifest_"+steamcmd.AppID+".acf"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstallArgsAndProgress(t *testing.T) {
	svc, calls := newFakeRunner(t, "Update state (0x61) downloading, progress: 50.0")
	gameDir := t.TempDir()

	var reports []string
	var lines []string
	err := svc.Install(context.Background(), gameDir,
		func(p int, msg string) { reports = append(reports, fmt.Sprintf("%d|%s", p, msg)) },
		func(line string) { lines = append(lines, line) })
	if err != nil {
		t.Fatal(err)
	}

	// ① Install 拼的参数包含 +force_install_dir / gameDir / +login anonymous / +app_update 2394010 validate +quit
	if len(*calls) != 1 {
		t.Fatalf("应恰好调用一次 SteamCMD，实际 %d 次: %v", len(*calls), *calls)
	}
	arg := (*calls)[0]
	for _, want := range []string{
		"+force_install_dir", gameDir, "+login", "anonymous",
		"+app_update", steamcmd.AppID, "validate", "+quit",
	} {
		if !strings.Contains(arg, want) {
			t.Fatalf("参数 %q 缺少 %q", arg, want)
		}
	}

	// ② 进度回调收到 50，消息为 downloading
	found := false
	for _, r := range reports {
		if r == "50|downloading" {
			found = true
		}
	}
	if !found {
		t.Fatalf("进度回调未收到 50|downloading: %v", reports)
	}
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "progress: 50.0") {
		t.Fatalf("onLine 未收到原始进度行: %v", lines)
	}
}

func TestInstallEnsureFailurePropagates(t *testing.T) {
	// 未预置 SteamCMD 入口且下载器始终失败：Install 应把 Ensure 错误透传
	dir := t.TempDir()
	r := steamcmd.NewRunner(dir)
	r.Command = func(ctx context.Context, args []string) *exec.Cmd {
		t.Fatal("SteamCMD 缺失时不应执行")
		return nil
	}
	svc := New(r, func(ctx context.Context, url, dest string) error { return errors.New("网络不可用") })
	if err := svc.Install(context.Background(), t.TempDir(), nil, nil); err == nil || !strings.Contains(err.Error(), "网络不可用") {
		t.Fatalf("应透传 Ensure 错误，实际 %v", err)
	}
}

func TestUpdateCheck(t *testing.T) {
	svc, calls := newFakeRunner(t, `"public" { "buildid" "25080279" }`)
	gameDir := t.TempDir()
	writeManifest(t, gameDir, "24575149")

	var lines []string
	local, remote, err := svc.UpdateCheck(context.Background(), gameDir, func(l string) { lines = append(lines, l) })
	if err != nil {
		t.Fatal(err)
	}
	if local != "24575149" || remote != "25080279" {
		t.Fatalf("local=%q remote=%q，期望 24575149/25080279", local, remote)
	}
	if len(*calls) != 1 || !strings.Contains((*calls)[0], "+app_info_print "+steamcmd.AppID) {
		t.Fatalf("应调用 app_info_print 2394010: %v", *calls)
	}
	if len(lines) == 0 {
		t.Fatal("onLine 未收到任何输出")
	}
}

func TestUpdateCheckRemoteUnavailable(t *testing.T) {
	svc, _ := newFakeRunner(t, "没有版本信息")
	gameDir := t.TempDir()
	writeManifest(t, gameDir, "24575149")

	local, remote, err := svc.UpdateCheck(context.Background(), gameDir, nil)
	if err == nil || !strings.Contains(err.Error(), "未能从 Steam 获取远端版本号") {
		t.Fatalf("应报远端版本缺失，实际 err=%v remote=%q", err, remote)
	}
	if local != "24575149" {
		t.Fatalf("local=%q，期望 24575149", local)
	}
}

func TestAdopt(t *testing.T) {
	svc, _ := newFakeRunner(t, "")

	// ③ 空目录 → ErrNotPalServer
	if _, err := svc.Adopt(t.TempDir()); !errors.Is(err, ErrNotPalServer) {
		t.Fatalf("空目录应返回 ErrNotPalServer，实际 %v", err)
	}

	// 服务端 exe + appmanifest(buildid 24575149) → 返回 "24575149"
	dir := t.TempDir()
	if err := os.WriteFile(ServerExePath(dir), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, dir, "24575149")
	id, err := svc.Adopt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id != "24575149" {
		t.Fatalf("buildID=%q，期望 24575149", id)
	}
}
