package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// agentOrder is the order amf tries agents in for an issue without a
// pipeline, when --agent doesn't name one.
var agentOrder = []string{agentClaude, agentCodex, agentOpencode}

// parseAgent reads an --agent value: claudecode (or claude), codex or
// opencode. "" means let amf pick (see defaultAgent).
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

// agentKeys are the environment variables that give an agent a credential
// amf can see. Without --agent, amf only picks claude or codex when one of
// its keys is set; opencode needs one only for a paid OPENCODE_MODEL (see
// opencodeProvider), and can use its saved login instead.
var agentKeys = map[string][]string{
	agentClaude:   {"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
	agentCodex:    {"OPENAI_API_KEY"},
	agentOpencode: {"OPENCODE_API_KEY"},
}

// opencodeSavedLogin is agentKey's name for opencode's own saved login.
const opencodeSavedLogin = "saved login"

// agentKey says which of agent's keys is set, if any, or opencodeSavedLogin.
// ok is true for an agent that needs none: opencode with a free model.
func (r *Runner) agentKey(agent string) (name string, ok bool) {
	switch agent {
	case agentClaude:
		if r.ClaudeToken != "" {
			return "CLAUDE_CODE_OAUTH_TOKEN", true
		}
		if r.AnthropicKey != "" {
			return "ANTHROPIC_API_KEY", true
		}
		return "", false
	case agentCodex:
		if r.OpenAIKey != "" {
			return "OPENAI_API_KEY", true
		}
		return "", false
	case agentOpencode:
		provider := opencodeProvider(r.OpencodeModel)
		switch {
		case r.OpencodeKey != "":
			return "OPENCODE_API_KEY", true
		case provider == "":
			return "", true
		case r.opencodeLogin(provider):
			return opencodeSavedLogin, true
		}
		return "", false
	}
	return "", true
}

// opencodeProvider is the OpenCode provider model belongs to when it needs
// OPENCODE_API_KEY or a saved login: opencode for pay-as-you-go Console
// models, opencode-go for OpenCode Go ones. It is "" for a model that needs
// neither: a free one (opencode/big-pickle, or an opencode/ id ending in
// -free), or another provider's, whose credentials opencode keeps itself.
func opencodeProvider(model string) string {
	provider, id, _ := strings.Cut(model, "/")
	switch {
	case provider == "opencode-go":
		return provider
	case provider == "opencode" && id != "big-pickle" && !strings.HasSuffix(id, "-free"):
		return provider
	}
	return ""
}

// opencodeLogin reports whether opencode has a saved credential for
// provider, as `opencode auth list --format json` lists them.
func (r *Runner) opencodeLogin(provider string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Opencode, "auth", "list", "--format", "json")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	var integrations []struct {
		ID          string `json:"id"`
		Connections []struct {
			Type string `json:"type"`
		} `json:"connections"`
	}
	if json.Unmarshal(out, &integrations) != nil {
		return false
	}
	for _, in := range integrations {
		for _, c := range in.Connections {
			if in.ID == provider && c.Type == "credential" {
				return true
			}
		}
	}
	return false
}

// agentStatus is what amf found for one agent: whether it's installed, and
// whether it has a key when it needs one.
type agentStatus struct {
	Agent, Bin string
	Path       string // where it's installed, or ""
	Key        string // the key variable that's set, or ""
	Keyed      bool   // has a key, or needs none
	Model      string // the model amf runs it with without a pipeline, or "" for its default
}

func (s agentStatus) usable() bool { return s.Path != "" && s.Keyed }

// reason says why amf passed over an agent that isn't usable.
func (s agentStatus) reason() string {
	if s.Path == "" {
		return s.Bin + " is not installed"
	}
	if s.Agent == agentOpencode {
		return "opencode has no OPENCODE_API_KEY or saved login for " + s.Model
	}
	return agentName(s.Agent) + " has no " + strings.Join(agentKeys[s.Agent], " or ")
}

// describe is one line for watch: the agent, where it's installed and its key.
func (s agentStatus) describe() string {
	where := "not installed"
	if s.Path != "" {
		where = "installed at " + s.Path
	}
	var key string
	switch {
	case s.Key == opencodeSavedLogin:
		key = "uses its saved login"
	case s.Key != "":
		key = s.Key + " is set"
	case s.Agent == agentOpencode && s.Keyed:
		key = "needs no key"
	case s.Agent == agentOpencode:
		key = "OPENCODE_API_KEY not set and no saved login"
	default:
		key = strings.Join(agentKeys[s.Agent], " and ") + " not set"
	}
	if s.Model != "" {
		key += ", model " + s.Model
	}
	verdict := "usable"
	if !s.usable() {
		verdict = "skipped"
	}
	return fmt.Sprintf("%-8s %s; %s: %s", agentName(s.Agent), where, key, verdict)
}

// agentStatuses checks each agent, in agentOrder. Agents with no binary
// configured (as in tests) are left out.
func (r *Runner) agentStatuses() []agentStatus {
	var out []agentStatus
	for _, a := range agentOrder {
		bin := r.agentBin(a)
		if bin == "" {
			continue
		}
		st := agentStatus{Agent: a, Bin: bin}
		if path, err := exec.LookPath(bin); err == nil {
			st.Path = path
		}
		st.Key, st.Keyed = r.agentKey(a)
		st.Model = r.agentModel(a, "")
		out = append(out, st)
	}
	return out
}

// defaultAgent picks the agent for an issue without a pipeline: the one
// --agent named, which must be installed, or else the first of claude,
// codex and opencode that is installed and has a key (see agentKeys).
func (r *Runner) defaultAgent() (string, error) {
	agent, _, err := r.pickAgent()
	return agent, err
}

// pickAgent is defaultAgent, also saying why each agent before the chosen
// one was passed over, such as "codex has no OPENAI_API_KEY".
func (r *Runner) pickAgent() (agent string, passed []string, err error) {
	if r.Agent != "" {
		if _, err := exec.LookPath(r.agentBin(r.Agent)); err != nil {
			return "", nil, fmt.Errorf("--agent %s: %s is not installed (%v)", r.Agent, r.agentBin(r.Agent), err)
		}
		return r.Agent, nil, nil
	}
	statuses := r.agentStatuses()
	if len(statuses) == 0 {
		return "", nil, fmt.Errorf("no coding agent is configured; set AMF_CLAUDE, AMF_CODEX or AMF_OPENCODE")
	}
	for _, st := range statuses {
		if st.usable() {
			return st.Agent, passed, nil
		}
		passed = append(passed, st.reason())
	}
	return "", passed, fmt.Errorf("no coding agent is available (%s); set ANTHROPIC_API_KEY or "+
		"CLAUDE_CODE_OAUTH_TOKEN for claude or OPENAI_API_KEY for codex, install opencode (with OPENCODE_API_KEY "+
		"for a paid OPENCODE_MODEL), or name one with --agent", strings.Join(passed, "; "))
}

// agentModel is the model to run agent with: model, if a pipeline stage
// names one, else OPENCODE_MODEL for opencode, else the agent's own default
// ("").
func (r *Runner) agentModel(agent, model string) string {
	if model == "" && agent == agentOpencode {
		return r.OpencodeModel
	}
	return model
}
