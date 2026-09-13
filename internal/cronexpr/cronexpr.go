// Package cronexpr 实现标准 5 字段（分 时 日 月 周）cron 表达式的解析与
// 下次触发时刻计算。纯函数、零第三方依赖，供定时备份/重启/广播调度使用。
//
// 语法：每字段支持 `*`、数字、`,` 列表、`-` 范围、`/` 步进（`*/5` 与
// `10-20/5` 两种形态）。取值范围：分钟 0-59、小时 0-23、日 1-31、
// 月 1-12、周 0-7（0 与 7 均为周日）。
//
// 语义遵循标准 vixie cron：日（DOM）与周（DOW）同时受限（均非 `*`）时
// 取并集（OR）；仅一个受限时取交集（AND）。
//
// Next 从 after 向下取整到分钟再加 1 分钟起逐分钟扫描，最多扫描
// 366*24*60 分钟；永不匹配（如 `0 0 31 2 *`）时返回零值 time.Time{}。
package cronexpr

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 扫描上限：一年多的分钟数，防止永不匹配的表达式导致死循环。
const maxScanMinutes = 366 * 24 * 60

// Schedule 是解析后的 cron 调度计划。值类型、字段未导出、不可变，
// 可并发复用；Next 不修改任何状态。
type Schedule struct {
	minute uint64 // 位集 bit0-59
	hour   uint64 // bit0-23
	dom    uint64 // bit1-31（日）
	month  uint64 // bit1-12
	dow    uint64 // bit0-6（周，0=周日）
	domSet bool   // 日字段受限（非 `*`）
	dowSet bool   // 周字段受限（非 `*`）
}

// Parse 解析标准 5 字段 cron 表达式，非法输入返回中文错误。
func Parse(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("表达式应为 5 个字段（分 时 日 月 周），实际 %d 个", len(fields))
	}

	var s Schedule
	var err error
	if s.minute, err = parseField(fields[0], 0, 59, "分钟"); err != nil {
		return Schedule{}, err
	}
	if s.hour, err = parseField(fields[1], 0, 23, "小时"); err != nil {
		return Schedule{}, err
	}
	if s.dom, err = parseField(fields[2], 1, 31, "日"); err != nil {
		return Schedule{}, err
	}
	if s.month, err = parseField(fields[3], 1, 12, "月"); err != nil {
		return Schedule{}, err
	}
	dow, err := parseField(fields[4], 0, 7, "周")
	if err != nil {
		return Schedule{}, err
	}
	// 0 与 7 都表示周日：把 bit7 折叠到 bit0。
	s.dow = dow | (dow >> 7)
	s.domSet = fields[2] != "*"
	s.dowSet = fields[4] != "*"
	return s, nil
}

// parseField 解析单个字段为位集，返回值中受限与否由调用方依据原始文本判断。
func parseField(field string, min, max int, name string) (uint64, error) {
	var mask uint64
	for _, item := range strings.Split(field, ",") {
		base, stepStr := item, ""
		hasStep := false
		if idx := strings.Index(item, "/"); idx >= 0 {
			hasStep = true
			base, stepStr = item[:idx], item[idx+1:]
			if strings.Contains(stepStr, "/") {
				return 0, fmt.Errorf("%s字段步长无效: %q", name, stepStr)
			}
		}

		step := 1
		if hasStep {
			v, err := strconv.Atoi(stepStr)
			if err != nil || v < 1 {
				return 0, fmt.Errorf("%s字段步长无效: %q（须为正整数）", name, stepStr)
			}
			step = v
		}

		var start, end int
		switch {
		case base == "*":
			start, end = min, max
		case strings.Contains(base, "-"):
			parts := strings.SplitN(base, "-", 2)
			var err error
			if start, err = parseNumber(parts[0], min, max, name); err != nil {
				return 0, err
			}
			if end, err = parseNumber(parts[1], min, max, name); err != nil {
				return 0, err
			}
			if start > end {
				return 0, fmt.Errorf("%s字段范围起点大于终点: %d-%d", name, start, end)
			}
		default:
			v, err := parseNumber(base, min, max, name)
			if err != nil {
				return 0, err
			}
			if hasStep {
				return 0, fmt.Errorf("%s字段不支持单值步进: %q（可用 * 或 a-b 配合步长）", name, item)
			}
			start, end = v, v
		}

		for i := start; i <= end; i += step {
			mask |= 1 << uint(i)
		}
	}
	return mask, nil
}

func parseNumber(tok string, min, max int, name string) (int, error) {
	v, err := strconv.Atoi(tok)
	if err != nil {
		return 0, fmt.Errorf("%s字段无效: %q", name, tok)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("%s字段越界: %d（允许 %d-%d）", name, v, min, max)
	}
	return v, nil
}

// Next 返回 after 之后的下一个触发时刻（本地时区，秒与纳秒为 0）。
// after 恰为触发时刻时返回下一个周期；永不匹配时返回零值 time.Time{}。
// after 不会被修改，Schedule 不可变，可安全并发调用。
func (s Schedule) Next(after time.Time) time.Time {
	loc := after.Location()
	t := time.Date(after.Year(), after.Month(), after.Day(),
		after.Hour(), after.Minute(), 0, 0, loc).Add(time.Minute)

	for i := 0; i < maxScanMinutes; i++ {
		if s.matches(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}

func (s Schedule) matches(t time.Time) bool {
	if s.minute&(1<<uint(t.Minute())) == 0 {
		return false
	}
	if s.hour&(1<<uint(t.Hour())) == 0 {
		return false
	}
	if s.month&(1<<uint(int(t.Month()))) == 0 {
		return false
	}
	domMatch := s.dom&(1<<uint(t.Day())) != 0
	dowMatch := s.dow&(1<<uint(int(t.Weekday()))) != 0
	// 标准 vixie cron 语义：日与周同时受限取并集；否则取交集。
	if s.domSet && s.dowSet {
		return domMatch || dowMatch
	}
	return domMatch && dowMatch
}
