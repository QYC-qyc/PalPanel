package palsettings

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Field 描述一个 PalWorld 配置项。
// Min/Max 为指针：nil 表示不设上下限（与"限制为 0"区分开）；
// 仅对 int/float 类型生效。
type Field struct {
	Key     string   // INI 键名，如 ExpRate
	Type    string   // int | float | bool | enum | string
	Default string   // 默认值（与官方 ini 输出格式一致，如 float 为 "1.000000"）
	Min     *float64 // 下限，nil 不限制
	Max     *float64 // 上限，nil 不限制
	Options []string // enum 合法取值
	Group   string   // 分组（前端展示用）
	Help    string   // 帮助文案（前端展示用）
}

// Fields 返回 schema 全部字段（按 fields.json 文件序）。
// 单例加载：首次调用解析嵌入的 fields.json，之后复用缓存。
// 返回内部数据的副本（Options 逐字段深拷贝），调用方改写不影响共享 schema。
func Fields() []Field {
	loadFields()
	if fieldsErr != nil {
		return nil
	}
	out := make([]Field, len(fieldsCache))
	for i, f := range fieldsCache {
		f.Options = append([]string(nil), f.Options...)
		out[i] = f
	}
	return out
}

// FieldByKey 按键名查找字段。
func FieldByKey(key string) (Field, bool) {
	loadFields()
	if fieldsErr != nil {
		return Field{}, false
	}
	f, ok := keyIndex[key]
	return f, ok
}

// stringMaxLen 限制 string 类型值的最大字符数（按 rune 计）。
const stringMaxLen = 128

// Validate 校验 value 是否符合字段定义。
// bool 入参大小写不敏感（true/TRUE 等均合法），输出归一在 Serialize 中处理。
// 错误信息为中文且包含字段 key。
func Validate(f Field, value string) error {
	switch f.Type {
	case "int":
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return fmt.Errorf("字段 %s 需为整数", f.Key)
		}
		return checkRange(f, float64(n))
	case "float":
		n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return fmt.Errorf("字段 %s 需为数值", f.Key)
		}
		return checkRange(f, n)
	case "bool":
		if strings.EqualFold(strings.TrimSpace(value), "true") ||
			strings.EqualFold(strings.TrimSpace(value), "false") {
			return nil
		}
		return fmt.Errorf("字段 %s 取值需为 True 或 False", f.Key)
	case "enum":
		for _, o := range f.Options {
			if o == value {
				return nil
			}
		}
		return fmt.Errorf("字段 %s 取值 %q 不在允许范围 %v 内", f.Key, value, f.Options)
	case "string":
		if utf8.RuneCountInString(value) > stringMaxLen {
			return fmt.Errorf("字段 %s 长度不能超过 %d 个字符", f.Key, stringMaxLen)
		}
		return nil
	default:
		return fmt.Errorf("字段 %s 类型 %q 不受支持", f.Key, f.Type)
	}
}

func checkRange(f Field, n float64) error {
	if f.Min != nil && n < *f.Min {
		return fmt.Errorf("字段 %s 不能小于 %v", f.Key, *f.Min)
	}
	if f.Max != nil && n > *f.Max {
		return fmt.Errorf("字段 %s 不能大于 %v", f.Key, *f.Max)
	}
	return nil
}
