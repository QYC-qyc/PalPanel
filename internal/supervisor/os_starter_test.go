package supervisor

import (
	"path/filepath"
	"runtime"
	"testing"

	"palpanel/internal/installer"
	"palpanel/internal/instance"
)

// ---- NewOSStarter 参数/dir 拼接（不实际启动进程；真机冒烟覆盖启动链路） ----

func TestOSServerCommandConstruction(t *testing.T) {
	inst := instance.Instance{
		GameDir:   filepath.Join(t.TempDir(), "PalServer"),
		GamePort:  8211,
		QueryPort: 27015,
	}
	cmd := osServerCommand(inst, ArgsFor(inst))

	if cmd.Dir != inst.GameDir {
		t.Fatalf("cmd.Dir=%q，want %q", cmd.Dir, inst.GameDir)
	}
	exe := installer.ServerExePath(inst.GameDir)
	if runtime.GOOS == "windows" {
		if cmd.Path != exe {
			t.Fatalf("windows cmd.Path=%q，want %q", cmd.Path, exe)
		}
		if cmd.Args[0] != exe || cmd.Args[1] != "-port=8211" {
			t.Fatalf("windows cmd.Args=%v", cmd.Args)
		}
	} else {
		if cmd.Path != "bash" {
			t.Fatalf("linux cmd.Path=%q，want bash", cmd.Path)
		}
		if cmd.Args[0] != "bash" || cmd.Args[1] != exe || cmd.Args[2] != "-port=8211" {
			t.Fatalf("linux cmd.Args=%v", cmd.Args)
		}
	}
}

// args 为空时回退 ArgsFor（防调用方漏传导致空参数启动）。
func TestOSServerCommandFallbackArgs(t *testing.T) {
	inst := instance.Instance{GameDir: "gd", GamePort: 8211, QueryPort: 27015}
	cmd := osServerCommand(inst, nil)
	want := ArgsFor(inst)
	if len(cmd.Args) < 1+len(want) {
		t.Fatalf("args 空时应回退 ArgsFor，cmd.Args=%v", cmd.Args)
	}
	for i, w := range want {
		if cmd.Args[1+i] != w {
			t.Fatalf("cmd.Args[%d]=%q，want %q", 1+i, cmd.Args[1+i], w)
		}
	}
}
