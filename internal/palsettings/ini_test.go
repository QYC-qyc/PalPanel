package palsettings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sampleINI = `; This configuration file is a sample of the default server settings.
; Changes to this setting will not be able to be reflected on the server.

[/Script/Engine.GameSession]
MaxPlayers=32
OptionSettings=(Fake=Before)

[/Script/Pal.PalGameWorldSettings]
OptionSettings=(Difficulty=None,ExpRate=1.000000,ServerName="Pal Test=1",ServerPassword="a,b",DeathPenalty=Item)
`

func TestParseINI(t *testing.T) {
	m, err := ParseINI(sampleINI)
	if err != nil {
		t.Fatalf("ParseINI: %v", err)
	}
	want := map[string]string{
		"Difficulty":     "None",
		"ExpRate":        "1.000000",
		"ServerName":     "Pal Test=1",
		"ServerPassword": "a,b",
		"DeathPenalty":   "Item",
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("ParseINI[%q] = %q, 期望 %q", k, m[k], v)
		}
	}
	if _, ok := m["Fake"]; ok {
		t.Fatal("段外 OptionSettings 不应被采集")
	}
	if _, ok := m["MaxPlayers"]; ok {
		t.Fatal("其他段键不应被采集")
	}
}

func TestParseINIEscapedQuote(t *testing.T) {
	m, err := ParseINI(`[/Script/Pal.PalGameWorldSettings]
OptionSettings=(ServerPassword="a""b",ServerName="")
`)
	if err != nil {
		t.Fatalf("ParseINI: %v", err)
	}
	if m["ServerPassword"] != `a"b` {
		t.Fatalf("ServerPassword = %q, 期望 %q", m["ServerPassword"], `a"b`)
	}
	if m["ServerName"] != "" {
		t.Fatalf("ServerName = %q, 期望空串", m["ServerName"])
	}
}

func TestParseINIMissingSectionOrOption(t *testing.T) {
	if _, err := ParseINI("[Foo]\nBar=1\n"); err == nil {
		t.Fatal("缺段应报错")
	}
	if _, err := ParseINI("[/Script/Pal.PalGameWorldSettings]\nFoo=1\n"); err == nil {
		t.Fatal("缺 OptionSettings 应报错")
	}
}

func TestSerializeOrderAndPreserve(t *testing.T) {
	overrides := map[string]string{"ExpRate": "2.5", "ServerName": "Pal Panel"}
	existing := map[string]string{
		"Difficulty": "Difficulty",
		"Zeta":       "z",
		"Alpha":      "a",
	}
	got := Serialize(overrides, existing)
	want := "OptionSettings=(Difficulty=Difficulty,ExpRate=2.500000,ServerName=Pal Panel,Alpha=a,Zeta=z)"
	if got != want {
		t.Fatalf("Serialize:\n got  %s\n want %s", got, want)
	}
}

func TestSerializeEmptyOverrideFallsBack(t *testing.T) {
	// 空 override 视为未设置，回退 existing → Default
	got := Serialize(map[string]string{"ServerName": ""}, map[string]string{})
	want := "OptionSettings=(Difficulty=None,ExpRate=1.000000,ServerName=)"
	if got != want {
		t.Fatalf("Serialize:\n got  %s\n want %s", got, want)
	}
}

func TestSerializeQuoteEscapeRoundTrip(t *testing.T) {
	v := `a"b,c=d`
	line := Serialize(map[string]string{"ServerName": v}, nil)
	if !strings.Contains(line, `ServerName="a""b,c=d"`) {
		t.Fatalf("应加引号并转义: %s", line)
	}
	full := "; header\n[/Script/Pal.PalGameWorldSettings]\n" + line + "\n"
	m, err := ParseINI(full)
	if err != nil {
		t.Fatalf("ParseINI: %v", err)
	}
	if m["ServerName"] != v {
		t.Fatalf("往返不一致: %q vs %q", m["ServerName"], v)
	}
}

func TestSerializeParseRoundTripQuoteEdge(t *testing.T) {
	cases := []string{
		`pass""word`, // 连续双引号
		`"`,          // 恰为单个引号
		`"a"`,        // 两端引号
		`a"b,c=d`,    // 引号+逗号+等号混合
		`a,b`,        // 纯逗号
	}
	for _, v := range cases {
		line := Serialize(map[string]string{"ServerName": v}, nil)
		full := "; h\n[/Script/Pal.PalGameWorldSettings]\n" + line + "\n"
		m, err := ParseINI(full)
		if err != nil {
			t.Fatalf("ParseINI(%q): %v", line, err)
		}
		if m["ServerName"] != v {
			t.Fatalf("往返不一致: 原值 %q, 序列化 %q, 解析回 %q", v, line, m["ServerName"])
		}
	}
}

func TestFormatValueBoolAndFloat(t *testing.T) {
	if got := formatValue(Field{Key: "b", Type: "bool"}, "true"); got != "True" {
		t.Fatalf("bool true → %q, 期望 True", got)
	}
	if got := formatValue(Field{Key: "b", Type: "bool"}, "FALSE"); got != "False" {
		t.Fatalf("bool FALSE → %q, 期望 False", got)
	}
	if got := formatValue(Field{Key: "f", Type: "float"}, "2.5"); got != "2.500000" {
		t.Fatalf("float → %q, 期望 2.500000", got)
	}
}

func TestValidateValueWithSchemaField(t *testing.T) {
	// fields.json 中的 ExpRate 走 schema 校验路径
	f, _ := FieldByKey("ExpRate")
	if err := Validate(f, "1.5"); err != nil {
		t.Fatalf("ExpRate=1.5 应合法: %v", err)
	}
	if err := Validate(f, "0"); err == nil {
		t.Fatal("ExpRate=0 应低于下限")
	}
}

const iniFileContent = "; header\n[/Script/Pal.PalGameWorldSettings]\nOptionSettings=(Difficulty=None,ExpRate=1.500000,ServerName=Panel)\n"

func TestSaveLoadGameINIRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := SaveGameINI(dir, iniFileContent); err != nil {
		t.Fatalf("SaveGameINI: %v", err)
	}
	m, err := LoadGameINI(dir)
	if err != nil {
		t.Fatalf("LoadGameINI: %v", err)
	}
	if m["ExpRate"] != "1.500000" || m["ServerName"] != "Panel" {
		t.Fatalf("往返结果不符: %v", m)
	}
	// 文件确实落在平台子目录
	platform := "LinuxServer"
	if filepath.Separator == '\\' {
		platform = "WindowsServer"
	}
	p := filepath.Join(dir, "Pal", "Saved", "Config", platform, "PalWorldSettings.ini")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("期望文件 %s: %v", p, err)
	}
}

func TestLoadGameININotExist(t *testing.T) {
	m, err := LoadGameINI(t.TempDir())
	if err != nil {
		t.Fatalf("文件不存在应返回 nil 错误: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("应返回空 map, got %v", m)
	}
}

func TestWorldOptionHint(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	past := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	future := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	// 无 WorldOption.sav → 空串
	if got := WorldOptionHint(dir, old); got != "" {
		t.Fatalf("无存档应返回空串, got %q", got)
	}

	sav := filepath.Join(dir, "Pal", "Saved", "SaveGames", "0", "ABC123", "WorldOption.sav")
	if err := os.MkdirAll(filepath.Dir(sav), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sav, []byte("gvas"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 存档比 ini 新 → 提示
	if got := WorldOptionHint(dir, past); got == "" {
		t.Fatal("存档更新时应返回提示")
	}
	// ini 比存档新 → 空串
	if got := WorldOptionHint(dir, future); got != "" {
		t.Fatalf("ini 更新时应返回空串, got %q", got)
	}
}
