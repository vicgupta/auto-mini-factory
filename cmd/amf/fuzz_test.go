package main

import (
	"strings"
	"testing"
)

func FuzzParseDotEnv(f *testing.F) {
	f.Add("KEY=value\nFOO=bar # comment\n")
	f.Add("export X=\"y\"\n")
	f.Fuzz(func(t *testing.T, data string) {
		_, _ = parseDotEnv(strings.NewReader(data))
	})
}

func FuzzNormalizeRepo(f *testing.F) {
	f.Add("owner/name")
	f.Add("https://github.com/owner/name.git")
	f.Fuzz(func(t *testing.T, data string) {
		_ = normalizeRepo(data)
	})
}
