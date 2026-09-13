// Package backup 提供游戏存档备份的打包（tar.gz）、校验与恢复能力。
//
// 设计要点：
//   - Create 把 sourceDir 整树按相对路径写入 tar（PAX 格式，保中文 UTF-8），
//     空目录不单独建条目，符号链接等特殊文件跳过；
//     extra 以 "_panel/<name>" 虚拟文件追加（Restore 时不落盘）。
//   - SHA256 对落盘后的最终文件二次读取计算，避免 MultiWriter 双写复杂度。
//   - ctx 取消/出错时关闭 writer 并删除半成品，返回 ErrCancelled 或原 err。
//   - Restore 先 Verify，再清空 targetDir 原有内容（防旧文件残留混合），
//     解包时做 Clean + 分隔符前缀守卫，防路径逃逸（zip-slip 教训）。
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	// ErrCorrupted 表示存档包损坏或不完整（gzip/tar 结构错、CRC 不符等）。
	ErrCorrupted = errors.New("backup: 存档包损坏或不完整")
	// ErrCancelled 表示操作因 ctx 取消而中止。
	ErrCancelled = errors.New("backup: 操作已取消")
)

// panelPrefix 是 Create 追加的虚拟文件的统一前缀，Restore 时跳过。
const panelPrefix = "_panel/"

// BackupInfo 描述一个备份包的基本信息。
type BackupInfo struct {
	File      string // 备份包路径
	SizeBytes int64  // 落盘文件大小
	FileCount int    // tar 内普通文件条目数（含 _panel/ 虚拟文件）
	SHA256    string // 对落盘文件整体计算的十六进制摘要
}

// Create 把 sourceDir 整树打进 gzip tar 写入 destFile，extra 追加为
// "_panel/<name>" 虚拟文件。FileCount 计源树普通文件与 extra 之和。
func Create(ctx context.Context, sourceDir, destFile string, extra map[string][]byte) (BackupInfo, error) {
	if err := ctx.Err(); err != nil {
		return BackupInfo{}, ErrCancelled
	}
	if err := os.MkdirAll(filepath.Dir(destFile), 0o755); err != nil {
		return BackupInfo{}, err
	}
	f, err := os.Create(destFile)
	if err != nil {
		return BackupInfo{}, err
	}
	aborted := true // 出错统一清理半成品
	defer func() {
		_ = f.Close()
		if aborted {
			_ = os.Remove(destFile)
		}
	}()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	info := BackupInfo{File: destFile}
	fail := func(err error) (BackupInfo, error) {
		if ctx.Err() != nil {
			return BackupInfo{}, ErrCancelled
		}
		return BackupInfo{}, err
	}

	addEntry := func(name string, size int64, mod time.Time, r io.Reader) error {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Size:     size,
			Mode:     0o644,
			ModTime:  mod,
			Format:   tar.FormatPAX, // 保中文/长文件名 UTF-8
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := io.Copy(tw, r)
		return err
	}

	// 整树相对路径条目；WalkDir 按字典序，条目顺序确定
	err = filepath.WalkDir(sourceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ErrCancelled
		}
		// 目录不单独建条目（空目录跳过）；符号链接等特殊文件跳过不跟随
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		fh, err := os.Open(path)
		if err != nil {
			return err
		}
		defer fh.Close()
		fi, err := fh.Stat()
		if err != nil {
			return err
		}
		if err := addEntry(filepath.ToSlash(rel), fi.Size(), fi.ModTime(), fh); err != nil {
			return err
		}
		info.FileCount++
		return nil
	})
	if err != nil {
		return fail(err)
	}

	// extra 以固定顺序追加为 _panel/<name> 虚拟文件
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ctx.Err() != nil {
			return fail(ErrCancelled)
		}
		data := extra[name]
		if err := addEntry(panelPrefix+name, int64(len(data)), time.Now(), bytes.NewReader(data)); err != nil {
			return fail(err)
		}
		info.FileCount++
	}

	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if err := gz.Close(); err != nil {
		return fail(err)
	}
	aborted = false

	// SHA256 对落盘文件二次读取计算
	sum, size, err := fileHash(destFile)
	if err != nil {
		aborted = true
		return BackupInfo{}, err
	}
	info.SizeBytes = size
	info.SHA256 = sum
	return info, nil
}

// Verify 流式校验 destFile 的 gzip/tar 完整性，并重算 SHA256 与条目数。
// 任一环节失败返回 ErrCorrupted（包不存在则返回底层 os 错误）。
func Verify(destFile string) (BackupInfo, error) {
	sum, size, err := fileHash(destFile)
	if err != nil {
		return BackupInfo{}, err
	}
	count, err := countEntries(destFile)
	if err != nil {
		return BackupInfo{}, err
	}
	return BackupInfo{File: destFile, SizeBytes: size, FileCount: count, SHA256: sum}, nil
}

// countEntries 读完整个 gzip tar 流：结构错误、CRC 不符、尾部缺失均视为 ErrCorrupted。
func countEntries(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	tr := tar.NewReader(gz)
	count := 0
	for {
		_, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrCorrupted, err)
		}
		count++
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return 0, fmt.Errorf("%w: %v", ErrCorrupted, err)
		}
	}
	// 排干 gzip 剩余流以校验 CRC/长度尾部（tar EOF 可能早于 gzip 流末尾）
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	if err := gz.Close(); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	return count, nil
}

// Restore 先 Verify，再清空 targetDir 原有内容，随后解包：
// "_panel/" 前缀条目跳过不落盘；每落盘一个条目回调 onEntry（传相对路径）。
func Restore(ctx context.Context, destFile, targetDir string, onEntry func(name string)) error {
	if _, err := Verify(destFile); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return ErrCancelled
	}
	// 清空原有内容，防旧文件残留与备份混合；RemoveAll 后重建目录
	if err := os.RemoveAll(targetDir); err != nil {
		return err
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}

	f, err := os.Open(destFile)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	tr := tar.NewReader(gz)
	root := filepath.Clean(targetDir)
	for {
		if err := ctx.Err(); err != nil {
			return ErrCancelled
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCorrupted, err)
		}
		name := hdr.Name
		// extra 虚拟文件不落盘
		if name == "_panel" || strings.HasPrefix(name, panelPrefix) {
			continue
		}
		// 防路径逃逸（zip-slip）：Clean 后拒绝绝对路径与 ..
		rel := filepath.Clean(filepath.FromSlash(name))
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		dest := filepath.Join(root, rel)
		if dest != root && !strings.HasPrefix(dest, root+string(filepath.Separator)) {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			continue
		case tar.TypeReg:
			// 落盘
		default:
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode).Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, tr)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if onEntry != nil {
			onEntry(filepath.ToSlash(name))
		}
	}
	return nil
}

// fileHash 对文件整体流式计算 SHA256，同时返回文件大小。
func fileHash(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}
