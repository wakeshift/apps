//go:build unix

package layouttest

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// ownGroup starts cmd in a process group of its own, which its children
// join.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills cmd's process group and waits (a few seconds at most) for
// every process in it to go.
func killGroup(cmd *exec.Cmd) {
	pgid := cmd.Process.Pid
	syscall.Kill(-pgid, syscall.SIGKILL)
	for range 50 {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
