//go:build !windows

package supervisor

import (
	"syscall"
)

// defaultKill POSIX 下 SIGKILL；进程已不存在视为成功。
func defaultKill(pid int) error {
	err := syscall.Kill(pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}
