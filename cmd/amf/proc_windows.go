//go:build windows

// Process-group helpers on Windows. Windows has no signal that addresses a
// group, so amf puts each agent in its own process group and, when a run
// must die, asks taskkill to terminate the whole tree. Every kill is a hard
// one; amf's SIGTERM/grace/SIGKILL escalation has no effect, and there is no
// graceful way to stop a tree.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// procGroup puts the agent in a new Windows process group.
func procGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killGroup terminates the agent's whole process tree, whatever sig asked
// for. Already-exited trees fail with an error callers ignore.
func killGroup(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return nil
	}
	out, err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("taskkill /T /F /PID %d: %v: %s", p.Pid, err, out)
	}
	return nil
}
