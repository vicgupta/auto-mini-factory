package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// agentOrder is the order amf tries agents in for an issue without a
// pipeline, when --agent doesn't name one.
var agentOrder = []string{agentClaude, agentCodex, agentOpencode}

// parseAgent reads an --agent value: claudecode (or claude), codex or
// opencode. "" means pick the first installed one.
func parseAgent(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case "claude", agentClaude:
		return agentClaude, nil
	case agentCodex:
		return agentCodex, nil
	case agentOpencode:
		return agentOpencode, nil
	}
	return "", fmt.Errorf("--agent must be claudecode, codex or opencode, got %q", s)
}

// agentName is the command an agent runs as, for messages: claude, codex
// or opencode.
func agentName(agent string) string {
	if name := agentCommand[agent]; name != "" {
		return name
	}
	return "claude"
}

// defaultAgent picks the agent for an issue without a pipeline: the one
// --agent named, which must be installed, or else the first installed of
// claude, codex and opencode.
func (r *Runner) defaultAgent() (string, error) {
	if r.Agent != "" {
		if _, err := exec.LookPath(r.agentBin(r.Agent)); err != nil {
			return "", fmt.Errorf("--agent %s: %s is not installed (%v)", r.Agent, r.agentBin(r.Agent), err)
		}
		return r.Agent, nil
	}
	var tried []string
	for _, agent := range agentOrder {
		bin := r.agentBin(agent)
		if bin == "" {
			continue // not configured, as in tests
		}
		if _, err := exec.LookPath(bin); err == nil {
			return agent, nil
		}
		tried = append(tried, bin)
	}
	if len(tried) == 0 {
		return "", fmt.Errorf("no coding agent is configured; set AMF_CLAUDE, AMF_CODEX or AMF_OPENCODE")
	}
	return "", fmt.Errorf("no coding agent is installed (tried %s); install claude, codex or opencode, "+
		"or point AMF_CLAUDE, AMF_CODEX or AMF_OPENCODE at one", strings.Join(tried, ", "))
}
