package supervisor

import (
	"fmt"
	"io"
	"os/exec"
	"runtime"

	"palpanel/internal/installer"
	"palpanel/internal/instance"
)

// osServerCommand 构造服务端启动命令（不启动）：Windows 直接执行 PalServer.exe，
// 其余平台经 bash 执行 PalServer.sh；工作目录为游戏目录。args 为空时回退 ArgsFor。
func osServerCommand(inst instance.Instance, args []string) *exec.Cmd {
	if len(args) == 0 {
		args = ArgsFor(inst)
	}
	exe := installer.ServerExePath(inst.GameDir)
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command(exe, args...)
	} else {
		cmd = exec.Command("bash", append([]string{exe}, args...)...)
	}
	cmd.Dir = inst.GameDir
	return cmd
}

// NewOSStarter 生产 Starter：os/exec 启动服务端进程，返回进程句柄与 stdout 日志流。
// 启动链路依赖真进程，不做单测（参数/dir 拼接由 osServerCommand 单测，
// 树杀/退出回收由真机冒烟覆盖）。
func NewOSStarter(inst instance.Instance, args []string) (RunningProcess, io.ReadCloser, error) {
	cmd := osServerCommand(inst, args)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("创建 stdout 管道: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("启动服务端进程: %w", err)
	}
	return osProcess{cmd: cmd}, out, nil
}

// osProcess 将 os/exec 命令适配为 RunningProcess。
type osProcess struct{ cmd *exec.Cmd }

func (p osProcess) Pid() int { return p.cmd.Process.Pid }

// Wait 等进程退出并回收资源（cmd.Wait 负责清理句柄）。
func (p osProcess) Wait() error { return p.cmd.Wait() }

// Kill 树杀：Windows 下 Go 的 Process.Kill 只杀父进程，PalServer 会残留
// 子进程（如 PalServer-Win64-Shipping-Cmd.exe），必须 taskkill /T /F；
// POSIX 下 SIGKILL（进程已存在与否见 defaultKill）。
func (p osProcess) Kill() error { return defaultKill(p.cmd.Process.Pid) }
