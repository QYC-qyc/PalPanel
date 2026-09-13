package palsettings

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFieldsSingletonAndOrder(t *testing.T) {
	a := Fields()
	b := Fields()
	if len(a) != 3 {
		t.Fatalf("Fields() 数量 = %d, 期望 3", len(a))
	}
	for i := range a {
		if a[i].Key != b[i].Key {
			t.Fatalf("Fields() 两次调用结果不一致: %q vs %q", a[i].Key, b[i].Key)
		}
	}
	// 按文件序：Difficulty, ExpRate, ServerName
	wantOrder := []string{"Difficulty", "ExpRate", "ServerName"}
	for i, want := range wantOrder {
		if a[i].Key != want {
			t.Fatalf("Fields()[%d].Key = %q, 期望 %q", i, a[i].Key, want)
		}
	}
}

func TestFieldsReturnsCopy(t *testing.T) {
	a := Fields()
	a[0].Key = "Tampered"
	a[0].Type = "int"
	b := Fields()
	if b[0].Key != "Difficulty" || b[0].Type != "enum" {
		t.Fatalf("Fields() 应返回副本防外部改写: got %q/%q", b[0].Key, b[0].Type)
	}
	if f, _ := FieldByKey("Difficulty"); f.Key != "Difficulty" {
		t.Fatal("FieldByKey 受到污染")
	}
}

func TestFieldByKey(t *testing.T) {
	f, ok := FieldByKey("ExpRate")
	if !ok {
		t.Fatal("FieldByKey(ExpRate) 应存在")
	}
	if f.Type != "float" {
		t.Fatalf("ExpRate.Type = %q, 期望 float", f.Type)
	}
	if f.Min == nil || *f.Min != 0.1 {
		t.Fatalf("ExpRate.Min = %v, 期望 0.1", f.Min)
	}
	if f.Max == nil || *f.Max != 20.0 {
		t.Fatalf("ExpRate.Max = %v, 期望 20.0", f.Max)
	}
	if _, ok := FieldByKey("NoSuchKey"); ok {
		t.Fatal("FieldByKey(NoSuchKey) 不应存在")
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
