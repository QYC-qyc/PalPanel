package cronexpr

import (
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q) 意外出错: %v", expr, err)
	}
	return s
}

func at(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.Local)
}

// TestParseError 非法输入必须报错且消息为中文。
func TestParseError(t *testing.T) {
	cases := []struct {
		expr    string
		wantSub string // 错误消息须包含的子串
	}{
		{"*/5 * * *", "字段"},       // 字段数不足
		{"*/5 * * * * *", "字段"},   // 6 字段非法
		{"", "字段"},                // 空表达式
		{"60 * * * *", "分钟字段"},    // 分钟越界
		{"*/0 * * * *", "步长"},     // 步长为 0
		{"-5 * * * *", "分钟字段"},    // 负数/范围起点越界
		{"0-70 * * * *", "分钟字段"},  // 范围终点越界
		{"25-10 * * * *", "范围"},   // 范围起点大于终点
		{"* 24 * * *", "小时字段"},    // 小时越界
		{"* * 32 * *", "日字段"},     // 日越界
		{"* * * 13 *", "月字段"},     // 月越界
		{"* * * * 8", "周字段"},      // 周越界
		{"5,x * * * *", "分钟字段"},   // 非数字
		{"1- * * * *", "分钟字段"},    // 残缺范围
		{"*/5/* * * * *", "分钟字段"}, // 多个斜杠
	}
	for _, tc := range cases {
		_, err := Parse(tc.expr)
		if err == nil {
			t.Errorf("Parse(%q) 应报错但返回 nil", tc.expr)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("Parse(%q) 错误消息 %q 未包含 %q", tc.expr, err.Error(), tc.wantSub)
		}
	}
}

// TestNext 表驱动验证 Next 语义。
func TestNext(t *testing.T) {
	cases := []struct {
		name  string
		expr  string
		after time.Time
		want  time.Time
	}{
		{
			name:  "每5分钟_after为整点恰好触发_取下一个周期",
			expr:  "*/5 * * * *",
			after: at(2026, 9, 14, 10, 0),
			want:  at(2026, 9, 14, 10, 5),
		},
		{
			name:  "每5分钟_after在间隙内",
			expr:  "*/5 * * * *",
			after: at(2026, 9, 14, 10, 2),
			want:  at(2026, 9, 14, 10, 5),
		},
		{
			name:  "每天03:30_跨日",
			expr:  "30 3 * * *",
			after: at(2026, 9, 14, 10, 0),
			want:  at(2026, 9, 15, 3, 30),
		},
		{
			name:  "每天03:30_after早于触发时刻_当天命中",
			expr:  "30 3 * * *",
			after: at(2026, 9, 14, 0, 0),
			want:  at(2026, 9, 14, 3, 30),
		},
		{
			name:  "每月1日_跨月",
			expr:  "0 0 1 * *",
			after: at(2026, 9, 14, 0, 0),
			want:  at(2026, 10, 1, 0, 0),
		},
		{
			name:  "工作日1-5_周五after_跳到下周一",
			expr:  "0 3 * * 1-5",
			after: at(2026, 9, 11, 3, 0), // 2026-09-11 是周五
			want:  at(2026, 9, 14, 3, 0), // 下周一
		},
		{
			name:  "DOM与DOW并集_13号或周五_周五先到",
			expr:  "0 0 13 * 5",
			after: at(2026, 9, 14, 0, 0), // 周一
			want:  at(2026, 9, 18, 0, 0), // 周五 9/18（13 号 9/13 已过）
		},
		{
			name:  "DOM与DOW并集_13号先到",
			expr:  "0 0 13 * 5",
			after: at(2026, 9, 1, 0, 0),
			want:  at(2026, 9, 4, 0, 0), // 周五 9/4 早于 13 号
		},
		{
			name:  "DOW周日0与7等价",
			expr:  "0 0 * * 7",
			after: at(2026, 9, 14, 0, 0),
			want:  at(2026, 9, 20, 0, 0), // 下周日
		},
		{
			name:  "范围步进10-20/5",
			expr:  "10-20/5 * * * *",
			after: at(2026, 9, 14, 10, 11),
			want:  at(2026, 9, 14, 10, 15),
		},
		{
			name:  "列表与范围混合",
			expr:  "0,15,30-40/10 6 * * *",
			after: at(2026, 9, 14, 5, 0),
			want:  at(2026, 9, 14, 6, 0),
		},
		{
			name:  "月份受限_跨年",
			expr:  "0 0 1 1 *",
			after: at(2026, 9, 14, 0, 0),
			want:  at(2027, 1, 1, 0, 0),
		},
		{
			name:  "Next不修改after秒数_返回值秒为0",
			expr:  "* * * * *",
			after: time.Date(2026, 9, 14, 10, 0, 37, 0, time.Local),
			want:  at(2026, 9, 14, 10, 1),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mustParse(t, tc.expr)
			before := tc.after
			got := s.Next(tc.after)
			if !got.Equal(tc.want) {
				t.Fatalf("Next(%s) = %s, 期望 %s", before.Format(time.RFC3339), got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
			if !tc.after.Equal(before) {
				t.Fatalf("Next 修改了 after: %s -> %s", before.Format(time.RFC3339), tc.after.Format(time.RFC3339))
			}
			if got.Second() != 0 || got.Nanosecond() != 0 {
				t.Fatalf("返回值应精确到分钟: %s", got.Format(time.RFC3339Nano))
			}
		})
	}
}

// TestNextNoMatch 永不匹配的表达式（2 月 31 日）须在扫描上限后报错而非死循环。
func TestNextNoMatch(t *testing.T) {
	s := mustParse(t, "0 0 31 2 *")
	got := s.Next(at(2026, 1, 1, 0, 0))
	var zero time.Time
	if !got.Equal(zero) {
		t.Fatalf("永不匹配应返回零值时刻, 实际 %s", got.Format(time.RFC3339))
	}
}

// TestParseImmutability 同一 Schedule 多次调用 Next 结果一致（不可变、可重入）。
func TestParseImmutability(t *testing.T) {
	s := mustParse(t, "*/5 * * * *")
	a := at(2026, 9, 14, 10, 0)
	if !s.Next(a).Equal(s.Next(a)) {
		t.Fatal("同一 Schedule 与同一 after 两次 Next 结果应一致")
	}
}
