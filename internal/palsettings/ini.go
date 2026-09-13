package palsettings

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const sectionHeader = "[/Script/Pal.PalGameWorldSettings]"

// ParseINI 从 PalWorldSettings.ini 全文中提取
// [/Script/Pal.PalGameWorldSettings] 段的 OptionSettings=(...) 键值对。
// 引号内逗号不切分（ServerPassword="a,b" 是单值）；值去包裹引号，
// 引号内 "" 转义还原为 "。
// 段外的 OptionSettings（其他段的同名键）不采集。
func ParseINI(content string) (map[string]string, error) {
	lines := strings.Split(content, "\n")
	inSection := false
	var body string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == sectionHeader {
			inSection = true
			continue
		}
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			inSection = false
			continue
		}
		if !inSection {
			continue
		}
		if i := strings.Index(t, "OptionSettings=("); i >= 0 {
			open := i + len("OptionSettings=(")
			closing := strings.LastIndex(t, ")")
			if closing < open {
				return nil, fmt.Errorf("OptionSettings 括号不匹配")
			}
			body = t[open:closing]
			break
		}
	}
	if body == "" && !inSection {
		return nil, errors.New(`未找到 [/Script/Pal.PalGameWorldSettings] 段`)
	}
	if body == "" {
		return nil, errors.New("未找到 OptionSettings 配置行")
	}
	return splitPairs(body), nil
}

// splitPairs 切分括号内内容为键值对，引号内逗号不切分。
func splitPairs(body string) map[string]string {
	m := make(map[string]string)
	tokens := splitRespectingQuotes(body)
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		eq := strings.Index(tok, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(tok[:eq])
		val := unquote(strings.TrimSpace(tok[eq+1:]))
		m[key] = val
	}
	return m
}

// splitRespectingQuotes 按逗号切分，双引号内不切分，"" 视为转义引号。
func splitRespectingQuotes(s string) []string {
	var tokens []string
	var sb strings.Builder
	inQuote := false
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '"':
			if inQuote && i+1 < len(runes) && runes[i+1] == '"' {
				sb.WriteRune('"')
				i++ // 跳过成对的第二个引号
			} else {
				inQuote = !inQuote
				sb.WriteRune(r)
			}
		case r == ',' && !inQuote:
			tokens = append(tokens, sb.String())
			sb.Reset()
		default:
			sb.WriteRune(r)
		}
	}
	tokens = append(tokens, sb.String())
	return tokens
}

// unquote 去除包裹引号。"" 转义还原已由 splitRespectingQuotes 在切分时
// 完成一次，这里只剥外壳，不再二次解码（否则连续 "" 会丢引号）。
func unquote(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return s[1 : len(s)-1]
	}
	return s
}

// quoteValue 对 string 值做对称转义：含逗号或引号时加引号并把 " 写作 ""。
func quoteValue(v string) string {
	if strings.ContainsAny(v, `,"`) {
		return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
	}
	return v
}

// Serialize 按 schema 键序生成 OptionSettings=(k=v,...) 一行。
// 取值优先级：overrides（非空才生效，空串视为未设置）→ existing → Default；
// existing 中 schema 外的键按字母序原样追加（保留未管理键）。
// bool 归一为 True/False，float 固定 6 位小数，含逗号/引号的值加引号转义（与 ParseINI 对称）。
func Serialize(overrides map[string]string, existing map[string]string) string {
	fields := Fields()
	parts := make([]string, 0, len(fields)+len(existing))
	managed := make(map[string]bool, len(fields))
	for _, f := range fields {
		managed[f.Key] = true
		v, ok := overrides[f.Key]
		if !ok || v == "" {
			v, ok = existing[f.Key]
		}
		if !ok || v == "" {
			v = f.Default
		}
		parts = append(parts, f.Key+"="+formatValue(f, v))
	}
	// 未管理键按字母序追加，保证输出确定
	var extra []string
	for k := range existing {
		if !managed[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		parts = append(parts, k+"="+quoteValue(existing[k]))
	}
	return "OptionSettings=(" + strings.Join(parts, ",") + ")"
}

// formatValue 按字段类型把内部值格式化为 ini 输出形式。
func formatValue(f Field, v string) string {
	switch f.Type {
	case "bool":
		if strings.EqualFold(strings.TrimSpace(v), "true") {
			return "True"
		}
		return "False"
	case "float":
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return fmt.Sprintf("%.6f", n)
		}
		return quoteValue(v)
	default:
		return quoteValue(v)
	}
}

// gameConfigRelPath 返回相对 gameDir 的 ini 路径，平台子目录按 GOOS 决定。
func gameConfigRelPath() []string {
	platform := "LinuxServer"
	if runtime.GOOS == "windows" {
		platform = "WindowsServer"
	}
	return []string{"Pal", "Saved", "Config", platform, "PalWorldSettings.ini"}
}

// LoadGameINI 读取 <gameDir>/Pal/Saved/Config/<Platform>/PalWorldSettings.ini
// 并解析为键值对。文件不存在返回空 map + nil（新实例首次无 ini 属合法状态）。
func LoadGameINI(gameDir string) (map[string]string, error) {
	p := filepath.Join(append([]string{gameDir}, gameConfigRelPath()...)...)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("读取 %s 失败: %w", p, err)
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return map[string]string{}, nil
	}
	return ParseINI(content)
}

// SaveGameINI 将 content 原样写入游戏 ini 路径（自动创建目录）。
func SaveGameINI(gameDir string, content string) error {
	p := filepath.Join(append([]string{gameDir}, gameConfigRelPath()...)...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", p, err)
	}
	return nil
}

// worldOptionHintText 世界选项存档比 ini 新时返回的中文提示。
const worldOptionHintText = "检测到世界选项存档（WorldOption.sav）比 PalWorldSettings.ini 更新：" +
	"游戏内可能已修改世界设置，直接覆盖配置可能被存档回写，建议停服后修改或先在游戏内确认。"

// WorldOptionHint 粗检测（不做 GVAS 解析）：存档目录
// <gameDir>/Pal/Saved/SaveGames/0/*/WorldOption.sav 任一存在且
// mtime 晚于 iniModTime 时返回中文提示，否则返回空串。
func WorldOptionHint(gameDir string, iniModTime time.Time) string {
	pattern := filepath.Join(gameDir, "Pal", "Saved", "SaveGames", "0", "*", "WorldOption.sav")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return ""
	}
	for _, p := range matches {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.ModTime().After(iniModTime) {
			return worldOptionHintText
		}
	}
	return ""
}
