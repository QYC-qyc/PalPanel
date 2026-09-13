package palsettings

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// minFieldCount 全量 schema 的键数下限（实机 ini 约 120 键）。
const minFieldCount = 100

func TestFieldsSingletonAndOrder(t *testing.T) {
	a := Fields()
	b := Fields()
	if len(a) < minFieldCount {
		t.Fatalf("Fields() 数量 = %d, 期望 ≥ %d", len(a), minFieldCount)
	}
	for i := range a {
		if a[i].Key != b[i].Key {
			t.Fatalf("Fields() 两次调用结果不一致: %q vs %q", a[i].Key, b[i].Key)
		}
	}
	// 键序按实机 ini 出现序：首键 Difficulty，尾键 bAllowEnemyCampSpawnNearBaseCamp
	if a[0].Key != "Difficulty" {
		t.Fatalf("Fields()[0].Key = %q, 期望 Difficulty", a[0].Key)
	}
	if last := a[len(a)-1].Key; last != "bAllowEnemyCampSpawnNearBaseCamp" {
		t.Fatalf("末键 = %q, 期望 bAllowEnemyCampSpawnNearBaseCamp", last)
	}
	seen := map[string]bool{}
	for _, f := range a {
		if f.Key == "" {
			t.Fatal("存在空 key")
		}
		if seen[f.Key] {
			t.Fatalf("键 %s 重复", f.Key)
		}
		seen[f.Key] = true
	}
}

// TestFieldsFullDataValidity 全量 schema 数据合法性：类型受支持、默认值规则。
func TestFieldsFullDataValidity(t *testing.T) {
	validTypes := map[string]bool{"int": true, "float": true, "bool": true, "enum": true, "string": true}
	for _, f := range Fields() {
		if !validTypes[f.Type] {
			t.Errorf("键 %s 类型 %q 不在 int/float/bool/enum/string 内", f.Key, f.Type)
		}
		// default 非空；实机确有空值键（ServerPassword/Region 等），
		// 放宽为 string 类型允许空串
		if f.Default == "" && f.Type != "string" {
			t.Errorf("键 %s（%s 类型）default 为空", f.Key, f.Type)
		}
		if f.Group == "" {
			t.Errorf("键 %s Group 为空", f.Key)
		}
		if f.Type == "enum" && len(f.Options) == 0 {
			t.Errorf("枚举键 %s 缺 Options", f.Key)
		}
	}
}

// TestFieldByKeySpotCheck 对实机 ini 代表键抽查（Task 8 指定）。
func TestFieldByKeySpotCheck(t *testing.T) {
	for key, wantType := range map[string]string{
		"Difficulty":       "string", // 枚举样文本按规则归为 string
		"DayTimeSpeedRate": "float",
		"ServerPassword":   "string",
	} {
		f, ok := FieldByKey(key)
		if !ok {
			t.Fatalf("FieldByKey(%s) 应存在", key)
		}
		if f.Type != wantType {
			t.Fatalf("%s.Type = %q, 期望 %q", key, f.Type, wantType)
		}
	}
	if f, _ := FieldByKey("DayTimeSpeedRate"); f.Default != "1.000000" {
		t.Fatalf("DayTimeSpeedRate.Default = %q, 期望 1.000000", f.Default)
	}
	if f, _ := FieldByKey("bEnableInvaderEnemy"); f.Type != "bool" || f.Default != "True" {
		t.Fatalf("bEnableInvaderEnemy = %q/%q, 期望 bool/True", f.Type, f.Default)
	}
	if _, ok := FieldByKey("NoSuchKey"); ok {
		t.Fatal("FieldByKey(NoSuchKey) 不应存在")
	}
}

func TestFieldsReturnsCopy(t *testing.T) {
	a := Fields()
	if len(a) == 0 {
		t.Fatal("Fields() 不应为空")
	}
	first := a[0].Key
	a[0].Key = "Tampered"
	a[0].Type = "int"
	b := Fields()
	if b[0].Key != first || b[0].Type == "int" {
		t.Fatalf("Fields() 应返回副本防外部改写: got %q/%q", b[0].Key, b[0].Type)
	}
	if f, _ := FieldByKey(first); f.Key != first {
		t.Fatal("FieldByKey 受到污染")
	}
}

func TestValidateInt(t *testing.T) {
	fInt := Field{Key: "MaxLevel", Type: "int", Min: ptr(1), Max: ptr(100)}
	cases := []struct {
		v     string
		valid bool
	}{
		{"1", true},
		{"50", true},
		{"100", true},
		{"0", false},
		{"101", false},
		{"5.5", false},
		{"abc", false},
		{"", false},
	}
	for _, c := range cases {
		err := Validate(fInt, c.v)
		if (err == nil) != c.valid {
			t.Fatalf("Validate(int %q) err = %v, 期望 valid=%v", c.v, err, c.valid)
		}
		if err != nil && !strings.Contains(err.Error(), "MaxLevel") {
			t.Fatalf("错误信息应含字段 key: %v", err)
		}
	}
}

func TestValidateFloat(t *testing.T) {
	f := Field{Key: "ExpRate", Type: "float", Min: ptr(0.1), Max: ptr(20)}
	cases := []struct {
		v     string
		valid bool
	}{
		{"0.1", true},
		{"1.5", true},
		{"20", true},
		{"0", false},
		{"20.1", false},
		{"abc", false},
		{"", false},
	}
	for _, c := range cases {
		err := Validate(f, c.v)
		if (err == nil) != c.valid {
			t.Fatalf("Validate(float %q) err = %v, 期望 valid=%v", c.v, err, c.valid)
		}
	}
	// 未设 Min/Max 时不限制
	fNoLimit := Field{Key: "Rate", Type: "float"}
	if err := Validate(fNoLimit, "-100"); err != nil {
		t.Fatalf("无 min/max 时不应限制: %v", err)
	}
}

func TestValidateBool(t *testing.T) {
	f := Field{Key: "RCONEnabled", Type: "bool"}
	for _, v := range []string{"True", "False", "true", "FALSE", "True "} {
		if err := Validate(f, strings.TrimSpace(v)); err != nil {
			t.Fatalf("Validate(bool %q) err = %v", v, err)
		}
	}
	for _, v := range []string{"yes", "1", "0", ""} {
		if err := Validate(f, v); err == nil {
			t.Fatalf("Validate(bool %q) 应失败", v)
		}
	}
}

func TestValidateEnum(t *testing.T) {
	f := Field{Key: "DeathPenalty", Type: "enum", Options: []string{"None", "Item", "ItemAndEquipment"}}
	if err := Validate(f, "Item"); err != nil {
		t.Fatalf("合法枚举值不应报错: %v", err)
	}
	err := Validate(f, "Hard")
	if err == nil || !strings.Contains(err.Error(), "DeathPenalty") {
		t.Fatalf("非法枚举值应报错且含 key: %v", err)
	}
}

func TestValidateString(t *testing.T) {
	f := Field{Key: "ServerName", Type: "string"}
	ok128 := strings.Repeat("测", 128)
	if err := Validate(f, ok128); err != nil {
		t.Fatalf("128 字符应合法: %v", err)
	}
	bad := strings.Repeat("测", 129)
	if utf8.RuneCountInString(bad) != 129 {
		t.Fatal("测试前置失败")
	}
	err := Validate(f, bad)
	if err == nil || !strings.Contains(err.Error(), "ServerName") {
		t.Fatalf("129 字符应报错且含 key: %v", err)
	}
}

func TestValidateUnknownType(t *testing.T) {
	f := Field{Key: "Weird", Type: "list"}
	if err := Validate(f, "x"); err == nil {
		t.Fatal("未知类型应报错")
	}
}

func ptr(f float64) *float64 { return &f }
