package steamcmd

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEnsureSkipsWhenExists(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ExeName()), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	var called bool
	if err := Ensure(context.Background(), dir, func(ctx context.Context, url, dest string) error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("downloader should not run when exe exists")
	}
}

// TestEnsureDownloadsAndExtracts：用注入的 Downloader 把一个含假 ExeName() 的 zip
// 复制到 Ensure 的下载目标，断言解压后目标目录出现 ExeName()。
func TestEnsureDownloadsAndExtracts(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "dl.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(file)
	fw, err := w.Create(ExeName())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if err := Ensure(context.Background(), filepath.Join(dir, "sc"), func(ctx context.Context, url, dest string) error {
		if !strings.Contains(url, "steamcmd") {
			t.Fatalf("url %q", url)
		}
		return copyFile(archive, dest)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sc", ExeName())); err != nil {
		t.Fatalf("exe not extracted: %v", err)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func TestRunStreamsLines(t *testing.T) {
	var lines []string
	r := NewRunner(t.TempDir())
	r.Command = func(ctx context.Context, args []string) *exec.Cmd {
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/c", "echo line1&& echo line2")
		}
		return exec.Command("sh", "-c", "printf 'line1\\nline2\\n'")
	}
	if err := r.Run(context.Background(), nil, func(s string) { lines = append(lines, s) }); err != nil {
		t.Fatal(err)
	}
	if len(lines) < 2 || lines[0] != "line1" {
		t.Fatalf("lines %v", lines)
	}
}
