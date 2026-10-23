//go:build unix

package main

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

// waitESRCH polls pid until it is gone, at most a few seconds.
func waitESRCH(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d outlived the timeout", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
