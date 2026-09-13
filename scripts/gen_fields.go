// gen_fields.go 是一次性开发工具：从实机 DefaultPalWorldSettings.ini
// 生成 internal/palsettings/fields.json 全量字段 schema。
//
// 用法：
//
//	go run ./scripts/gen_fields.go -in <实机ini路径> [-out internal/palsettings/fields.json]
//
// 解析复用 internal/palsettings.ParseINI（引号内逗号不切分、"" 转义），
// 本工具只负责：括号列表值规范化、按 ini 出现序遍历键、逐键推断类型。
// 不手工编造任何键——全部来自实机 ini。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"palpanel/internal/palsettings"
)

// 实机 ini 中 CrossplayPlatforms=(Steam,Xbox,PS5,Mac) 的值含未加引号的逗号，
// ParseINI 会按逗号切断；生成前先把 =(…) 列表值规范化为带引号形式，
// 使 ParseINI 能完整解析（输出时 Serialize 的 quoteValue 也会对称加引号）。
var reParenValue = regexp.MustCompile(`=(\([^()]*\))`)

var (
	reInt   = regexp.MustCompile(`^-?\d+$`)
	reFloat = regexp.MustCompile(`^-?\d+\.\d+$`)
)

// outField 与 internal/palsettings 的 rawField JSON 结构对齐。
type outField struct {
	Key     string   `json:"key"`
	Type    string   `json:"type"`
	Default string   `json:"default"`
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Options []string `json:"options,omitempty"`
	Group   string   `json:"group"`
	Help    string   `json:"help"`
}

func main() {
	in := flag.String("in", "", "实机 DefaultPalWorldSettings.ini 路径")
	out := flag.String("out", "internal/palsettings/fields.json", "输出 fields.json 路径")
	flag.Parse()
	if *in == "" {
		fmt.Fprintln(os.Stderr, "必须提供 -in <实机ini路径>")
		os.Exit(2)
	}

	raw, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 ini 失败: %v\n", err)
		os.Exit(1)
	}
	content := reParenValue.ReplaceAllString(string(raw), `="$1"`)

	// 值的解析统一交给 palsettings.ParseINI（复用，不重写）
	parsed, err := palsettings.ParseINI(content)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ParseINI 失败: %v\n", err)
		os.Exit(1)
	}

	// 按出现序遍历键（ParseINI 返回 map 无序），值仍以 ParseINI 结果为准并交叉校验
	pairs, err := scanOrderedPairs(optionSettingsBody(content))
	if err != nil {
		fmt.Fprintf(os.Stderr, "遍历 OptionSettings 失败: %v\n", err)
		os.Exit(1)
	}
	if len(pairs) != len(parsed) {
		fmt.Fprintf(os.Stderr, "键数不一致: 出现序 %d vs ParseINI %d\n", len(pairs), len(parsed))
		os.Exit(1)
	}

	fields := make([]outField, 0, len(pairs))
	seen := map[string]bool{}
	typeCount := map[string]int{}
	for _, p := range pairs {
		k, scanned := p[0], p[1]
		if seen[k] {
			fmt.Fprintf(os.Stderr, "键 %s 重复出现\n", k)
			os.Exit(1)
		}
		seen[k] = true
		v, ok := parsed[k]
		if !ok {
			fmt.Fprintf(os.Stderr, "键 %s 未被 ParseINI 采集\n", k)
			os.Exit(1)
		}
		if v != scanned {
			fmt.Fprintf(os.Stderr, "键 %s 值不一致: ParseINI %q vs 扫描 %q\n", k, v, scanned)
			os.Exit(1)
		}
		t := inferType(v)
		typeCount[t]++
		fields = append(fields, outField{
			Key:     k,
			Type:    t,
			Default: v,
			Group:   "通用",
		})
	}

	if err := writeJSON(*out, fields); err != nil {
		fmt.Fprintf(os.Stderr, "写出 %s 失败: %v\n", *out, err)
		os.Exit(1)
	}

	fmt.Printf("共 %d 键 → %s\n", len(fields), *out)
	for _, t := range []string{"bool", "int", "float", "string"} {
		fmt.Printf("  %-6s %d\n", t, typeCount[t])
	}
	var strKeys []string
	for _, f := range fields {
		if f.Type == "string" {
			strKeys = append(strKeys, f.Key)
		}
	}
	fmt.Printf("string 类键: %s\n", strings.Join(strKeys, ", "))
}

// optionSettingsBody 提取 OptionSettings=(...) 括号内内容（单行）。
func optionSettingsBody(content string) string {
	i := strings.Index(content, "OptionSettings=(")
	if i < 0 {
		return ""
	}
	line := content[i:]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	open := strings.IndexByte(line, '(')
	closing := strings.LastIndexByte(line, ')')
	if open < 0 || closing <= open {
		return ""
	}
	return line[open+1 : closing]
}

// scanOrderedPairs 按出现序提取键值对：引号内逗号不切分、"" 转义还原，
// 与 ParseINI 的语义一致；仅用于补足 map 丢失的顺序，值经 ParseINI 交叉校验。
func scanOrderedPairs(body string) ([][2]string, error) {
	var out [][2]string
	i, n := 0, len(body)
	for i < n {
		for i < n && !isKeyChar(body[i]) {
			i++
		}
		if i >= n {
			break
		}
		ks := i
		for i < n && isKeyChar(body[i]) {
			i++
		}
		key := body[ks:i]
		if i >= n || body[i] != '=' {
			continue // 非键值片段，跳过
		}
		i++
		var sb strings.Builder
		if i < n && body[i] == '"' {
			i++
			for i < n {
				if body[i] == '"' {
					if i+1 < n && body[i+1] == '"' {
						sb.WriteByte('"')
						i += 2
						continue
					}
					i++
					break
				}
				sb.WriteByte(body[i])
				i++
			}
		} else {
			for i < n && body[i] != ',' {
				sb.WriteByte(body[i])
				i++
			}
		}
		if i < n && body[i] == ',' {
			i++
		}
		out = append(out, [2]string{key, sb.String()})
	}
	return out, nil
}

func isKeyChar(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// inferType 按任务规则推断类型：True/False→bool；纯整数→int；
// 纯小数→float；其余（含引号/空/枚举样文本/列表）→string。
func inferType(v string) string {
	switch {
	case v == "True" || v == "False":
		return "bool"
	case reInt.MatchString(v):
		return "int"
	case reFloat.MatchString(v):
		return "float"
	default:
		return "string"
	}
}

func writeJSON(path string, fields []outField) error {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false) // BanListURL 等含 & 的值保持原样输出 UTF-8
	enc.SetIndent("", "  ")
	if err := enc.Encode(fields); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}
