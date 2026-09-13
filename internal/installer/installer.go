package installer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"palpanel/internal/steamcmd"
)

// ErrNotPalServer 表示目标目录缺少帕鲁服务端可执行文件，无法接管。
var ErrNotPalServer = errors.New("目标目录不是帕鲁服务端（未找到服务端可执行文件）")

// Service 编排 SteamCMD 的安装/接管/更新检查。
type Service struct {
	Runner *steamcmd.Runner
	DL     steamcmd.Downloader
}

func New(runner *steamcmd.Runner, dl steamcmd.Downloader) *Service {
	return &Service{Runner: runner, DL: dl}
}

// EnsureSteamCmd 保证 Runner.Dir 下存在 SteamCMD 可执行入口（缺失则下载解压）。
func (s *Service) EnsureSteamCmd(ctx context.Context) error {
	return steamcmd.Ensure(ctx, s.Runner.Dir, s.DL)
}

// ServerExePath 返回游戏目录下的服务端入口（windows: PalServer.exe / 其他: PalServer.sh）。
func ServerExePath(gameDir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(gameDir, "PalServer.exe")
	}
	return filepath.Join(gameDir, "PalServer.sh")
}

// Install 用 SteamCMD 下载/校验帕鲁服务端到 gameDir，
// 进度（解析 Update state 行）经 report 上报，原始输出行经 onLine 透传。
func (s *Service) Install(ctx context.Context, gameDir string, report func(progress int, message string), onLine func(string)) error {
	if report == nil {
		report = func(int, string) {}
	}
	report(3, "准备 SteamCMD")
	if err := s.EnsureSteamCmd(ctx); err != nil {
		return err
	}
	report(10, "开始下载/校验服务端")
	args := []string{
		"+force_install_dir", gameDir,
		"+login", "anonymous",
		"+app_update", steamcmd.AppID, "validate",
		"+quit",
	}
	return s.Runner.Run(ctx, args, func(line string) {
		if onLine != nil {
			onLine(line)
		}
		if p, state, ok := steamcmd.ParseProgressLine(line); ok {
			report(p, state)
		}
	})
}

// Adopt 校验 gameDir 是否为已安装的帕鲁服务端（服务端入口 + appmanifest），
// 成功返回清单中的 buildid；服务端入口缺失返回 ErrNotPalServer。
func (s *Service) Adopt(gameDir string) (string, error) {
	if _, err := os.Stat(ServerExePath(gameDir)); err != nil {
		return "", ErrNotPalServer
	}
	return steamcmd.ManifestBuildID(gameDir)
}

// UpdateCheck 对比本地 appmanifest buildid 与 Steam 远端 buildid，
// 远端取自 `+login anonymous +app_info_print <AppID>` 输出中 public 分支的值。
func (s *Service) UpdateCheck(ctx context.Context, gameDir string, onLine func(string)) (string, string, error) {
	local, err := steamcmd.ManifestBuildID(gameDir)
	if err != nil {
		return "", "", err
	}
	var remote string
	args := []string{"+login", "anonymous", "+app_info_print", steamcmd.AppID, "+quit"}
	err = s.Runner.Run(ctx, args, func(line string) {
		if onLine != nil {
			onLine(line)
		}
		if remote == "" {
			remote = steamcmd.ParseAppInfoBuildID(line)
		}
	})
	if err != nil {
		return local, "", err
	}
	if remote == "" {
		return local, "", errors.New("未能从 Steam 获取远端版本号")
	}
	return local, remote, nil
}
