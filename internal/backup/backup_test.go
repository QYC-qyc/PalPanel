package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// makeSourceTree 构造标准测试源树：
//
//	saves/Level.sav、saves/Players/x.sav、根级 1.txt、中文 存档说明.txt
func makeSourceTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	write("saves/Level.sav", "LEVEL-DATA-存档")
	write("saves/Players/x.sav", "player-save-bytes")
	write("1.txt", "hello pal")
	write("存档说明.txt", "中文内容说明")
	return dir
}

func mustCreate(t *testing.T, src, dest string, extra map[string][]byte) BackupInfo {
	t.Helper()
	info, err := Create(context.Background(), src, dest, extra)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return info
}

func listEntries(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		names = append(names, hdr.Name)
	}
	sort.Strings(names)
	return names
}

// 组 1：打包 → Verify 往返信息一致（含 extra 虚拟文件、PAX 中文条目）
func TestCreateVerifyRoundTrip(t *testing.T) {
	src := makeSourceTree(t)
	dest := filepath.Join(t.TempDir(), "out", "backup.tar.gz")
	extra := map[string][]byte{"panel-meta.json": []byte(`{"name":"测试实例"}`)}

	info := mustCreate(t, src, dest, extra)

	if info.File != dest {
		t.Errorf("File = %q, want %q", info.File, dest)
	}
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat dest: %v", err)
	}
	if info.SizeBytes != fi.Size() {
		t.Errorf("SizeBytes = %d, want %d", info.SizeBytes, fi.Size())
	}
	// 4 个源文件 + 1 个 extra
	if info.FileCount != 5 {
		t.Errorf("FileCount = %d, want 5", info.FileCount)
	}
	// SHA256 必须与对落盘文件二次读取重算的结果一致
	f, err := os.Open(dest)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash dest: %v", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != info.SHA256 {
		t.Errorf("SHA256 = %s, want %s", info.SHA256, got)
	}

	got, err := Verify(dest)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != info {
		t.Errorf("Verify info = %+v, want %+v", got, info)
	}

	// 条目应为相对路径 + _panel/ 虚拟文件，且中文文件名完好
	names := listEntries(t, dest)
	want := []string{"_panel/panel-meta.json", "1.txt", "saves/Level.sav", "saves/Players/x.sav", "存档说明.txt"}
	sort.Strings(want)
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("entries = %v, want %v", names, want)
	}
}

// 组 2：Restore 到空目录，内容逐字节一致；_panel 虚拟文件不落盘
func TestRestoreByteIdentical(t *testing.T) {
	src := makeSourceTree(t)
	dest := filepath.Join(t.TempDir(), "backup.tar.gz")
	extra := map[string][]byte{"panel-meta.json": []byte(`{}`)}
	mustCreate(t, src, dest, extra)

	target := filepath.Join(t.TempDir(), "restore")
	var seen []string
	err := Restore(context.Background(), dest, target, func(name string) {
		seen = append(seen, name)
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// 源树每个文件逐字节一致
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		want, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(target, rel))
		if err != nil {
			return err
		}
		if !bytes.Equal(want, got) {
			t.Errorf("file %s content mismatch", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("compare tree: %v", err)
	}

	// 还原树中不能多出文件（_panel 不落盘）
	var restored []string
	err = filepath.WalkDir(target, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(target, path)
			restored = append(restored, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk target: %v", err)
	}
	sort.Strings(restored)
	wantRestored := []string{"1.txt", "saves/Level.sav", "saves/Players/x.sav", "存档说明.txt"}
	if strings.Join(restored, "|") != strings.Join(wantRestored, "|") {
		t.Errorf("restored = %v, want %v", restored, wantRestored)
	}
	if _, err := os.Stat(filepath.Join(target, "_panel")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("_panel should not exist in target, stat err = %v", err)
	}
	// onEntry 只回调实际落盘条目
	if len(seen) != len(wantRestored) {
		t.Errorf("onEntry count = %d, want %d (%v)", len(seen), len(wantRestored), seen)
	}
}

// 组 3：Restore 前预置的杂物文件/目录被清空，不与备份内容混合
func TestRestoreClearsJunk(t *testing.T) {
	src := makeSourceTree(t)
	dest := filepath.Join(t.TempDir(), "backup.tar.gz")
	mustCreate(t, src, dest, nil)

	target := t.TempDir()
	junk1 := filepath.Join(target, "stale.sav")
	if err := os.WriteFile(junk1, []byte("old-junk"), 0o644); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	junkDir := filepath.Join(target, "Worlds", "nested")
	if err := os.MkdirAll(junkDir, 0o755); err != nil {
		t.Fatalf("mkdir junk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(junkDir, "junk.dat"), []byte("junk"), 0o644); err != nil {
		t.Fatalf("write junk: %v", err)
	}

	if err := Restore(context.Background(), dest, target, nil); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(junk1); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("junk file survived: %v", err)
	}
	if _, err := os.Stat(junkDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("junk dir survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "saves", "Level.sav")); err != nil {
		t.Errorf("restored file missing: %v", err)
	}
}

// 组 4：损坏（截断 / 字节翻转）→ ErrCorrupted
func TestVerifyCorrupted(t *testing.T) {
	src := makeSourceTree(t)
	dest := filepath.Join(t.TempDir(), "backup.tar.gz")
	mustCreate(t, src, dest, nil)

	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	t.Run("truncated", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "trunc.tar.gz")
		if err := os.WriteFile(bad, raw[:len(raw)/2], 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := Verify(bad); !errors.Is(err, ErrCorrupted) {
			t.Errorf("err = %v, want ErrCorrupted", err)
		}
	})
	t.Run("bit-flip", func(t *testing.T) {
		mutated := bytes.Clone(raw)
		mutated[len(mutated)/2] ^= 0xFF
		bad := filepath.Join(t.TempDir(), "flip.tar.gz")
		if err := os.WriteFile(bad, mutated, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := Verify(bad); !errors.Is(err, ErrCorrupted) {
			t.Errorf("err = %v, want ErrCorrupted", err)
		}
	})
}

// 组 5：ctx 取消 → ErrCancelled 且无半成品残留
func TestCancelled(t *testing.T) {
	src := makeSourceTree(t)

	t.Run("create-cancelled-no-leftover", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "backup.tar.gz")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Create(ctx, src, dest, nil); !errors.Is(err, ErrCancelled) {
			t.Errorf("err = %v, want ErrCancelled", err)
		}
		if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("half-written file left behind, stat err = %v", err)
		}
	})
	t.Run("restore-cancelled", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "backup.tar.gz")
		mustCreate(t, src, dest, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		target := filepath.Join(t.TempDir(), "target")
		if err := Restore(ctx, dest, target, nil); !errors.Is(err, ErrCancelled) {
			t.Errorf("err = %v, want ErrCancelled", err)
		}
	})
}

// 组 6：嵌套目录 + 中文文件名经 PAX 往返无损（独立断言）
func TestChineseNamesAndNestedDirs(t *testing.T) {
	src := makeSourceTree(t)
	dest := filepath.Join(t.TempDir(), "backup.tar.gz")
	mustCreate(t, src, dest, nil)

	target := filepath.Join(t.TempDir(), "restore")
	if err := Restore(context.Background(), dest, target, nil); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for rel, want := range map[string]string{
		"saves/Level.sav":     "LEVEL-DATA-存档",
		"saves/Players/x.sav": "player-save-bytes",
		"1.txt":               "hello pal",
		"存档说明.txt":            "中文内容说明",
	} {
		got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("file %s = %q, want %q", rel, got, want)
		}
	}
}
