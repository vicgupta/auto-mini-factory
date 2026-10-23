//go:build unix

// Process-group helpers for killing an agent: every agent runs in its own
// process group, so a timeout or Ctrl-C can stop it and everything it
// spawned (MCP servers, shells, test runners), not just the direct child.
package main

import (
	"os"
	"os/exec"
	"syscall"
)

// procGroup puts the agent in its own process group.
func procGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup signals the agent's process group, p's pid being the group. An
// empty group returns an error, which callers ignore: signalling it only
// makes sure nothing outlives the run.
func killGroup(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return nil
	}
	return syscall.Kill(-p.Pid, sig)
}
