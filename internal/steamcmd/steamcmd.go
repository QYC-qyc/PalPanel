package steamcmd

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ExeName 返回 SteamCMD 可执行入口名（windows: steamcmd.exe / 其他: steamcmd.sh）。
func ExeName() string {
	if runtime.GOOS == "windows" {
		return "steamcmd.exe"
	}
	return "steamcmd.sh"
}

// MirrorURLs 按优先级返回 SteamCMD 分发包下载地址（官方 + 国内镜像）。
func MirrorURLs() []string {
	if runtime.GOOS == "windows" {
		return []string{
			"https://steamcdn-a.akamaihd.net/client/steamcmd.zip",
			"https://media.st.dl.eccdnx.com/client/steamcmd.zip",
		}
	}
	return []string{
		"https://steamcdn-a.akamaihd.net/client/steamcmd_linux.tar.gz",
		"https://media.st.dl.eccdnx.com/client/steamcmd_linux.tar.gz",
	}
}

// Downloader 把 url 下载到 dest（由调用方注入，便于测试与超时控制）。
type Downloader func(ctx context.Context, url, dest string) error

// Ensure 保证 dir 下存在 SteamCMD 可执行入口：已存在直接返回；
// 否则按镜像序下载并解压，全部失败时返回最后一个错误。
func Ensure(ctx context.Context, dir string, dl Downloader) error {
	if _, err := os.Stat(filepath.Join(dir, ExeName())); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var lastErr error
	for _, url := range MirrorURLs() {
		archive := filepath.Join(dir, "steamcmd_download.bin")
		if err := dl(ctx, url, archive); err != nil {
			lastErr = err
			continue
		}
		if err := extract(archive, dir); err != nil {
			lastErr = err
			continue
		}
		os.Remove(archive)
		if _, err := os.Stat(filepath.Join(dir, ExeName())); err == nil {
			return nil
		}
		lastErr = errors.New("解压后未找到 " + ExeName())
	}
	return fmt.Errorf("SteamCMD 下载失败: %w", lastErr)
}

// extract 解压 SteamCMD 分发包（zip 或 tar.gz）到 dest。
// 格式按文件魔数嗅探（PK\x03\x04 为 zip，否则按 tar.gz），
// 对包内路径做 zip-slip 守卫（Clean + HasPrefix）。
func extract(archive, dest string) error {
	head := make([]byte, 4)
	if f, err := os.Open(archive); err == nil {
		_, _ = io.ReadFull(f, head)
		f.Close()
	}
	isZip := string(head) == "PK\x03\x04"
	if isZip {
		return extractZip(archive, dest)
	}
	return extractTarGz(archive, dest)
}

// containedIn 判断 target 是否位于 dest 之内（含等于），用于 zip-slip 守卫。
// 不用裸 HasPrefix：dest=…\sc 时兄弟目录 …\scx 也会命中前缀，必须补路径分隔符比较。
func containedIn(target, dest string) bool {
	dest = filepath.Clean(dest)
	target = filepath.Clean(target)
	if target == dest {
		return true
	}
	return strings.HasPrefix(target, dest+string(filepath.Separator))
}

func extractZip(archive, dest string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		target := filepath.Join(dest, f.Name)
		if !containedIn(target, dest) {
			continue // zip-slip 守卫
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		src, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			src.Close()
			return err
		}
		_, err = io.Copy(out, src)
		out.Close()
		src.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, hdr.Name)
		if !containedIn(target, dest) {
			continue // zip-slip 守卫
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		out.Close()
		if err != nil {
			return err
		}
	}
}

// Runner 以注入的 Command 构造并流式执行 SteamCMD。
type Runner struct {
	Dir     string
	Command func(ctx context.Context, args []string) *exec.Cmd
}

// NewRunner 返回默认 Runner：Windows 直接执行 steamcmd.exe，
// 其他平台经 `bash steamcmd.sh args` 执行。
func NewRunner(dir string) *Runner {
	r := &Runner{Dir: dir}
	r.Command = func(ctx context.Context, args []string) *exec.Cmd {
		if runtime.GOOS == "windows" {
			return exec.CommandContext(ctx, filepath.Join(dir, ExeName()), args...)
		}
		full := append([]string{filepath.Join(dir, ExeName())}, args...)
		return exec.CommandContext(ctx, "bash", full...)
	}
	return r
}

// Run 执行 SteamCMD，stdout+stderr 经 io.Pipe 合并后逐行回调 onLine，
// 扫描 goroutine 结束后才返回。
func (r *Runner) Run(ctx context.Context, args []string, onLine func(string)) error {
	cmd := r.Command(ctx, args)
	cmd.Dir = r.Dir
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	scanErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if onLine != nil {
				onLine(strings.TrimRight(sc.Text(), "\r"))
			}
		}
		scanErr <- sc.Err()
	}()
	runErr := cmd.Run()
	pw.Close()
	<-scanErr
	if runErr != nil && onLine != nil {
		onLine("进程退出: " + runErr.Error())
	}
	return runErr
}
