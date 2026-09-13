//go:build windows

package supervisor

import (
	"os/exec"
	"strconv"
)

// defaultKill Windows 下用 taskkill /T /F 树杀（Go 的 Process.Kill 不杀子进程）。
func defaultKill(pid int) error {
	return exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}
