//go:build windows

package main

import "testing"

// waitESRCH has no Windows version: the tests that call it skip on Windows
// before they get there, since process groups are a unix mechanism.
func waitESRCH(t *testing.T, pid int) {
	t.Helper()
	t.Skip("process groups are a unix mechanism")
}
