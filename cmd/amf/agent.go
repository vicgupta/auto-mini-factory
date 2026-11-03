package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// agentOrder is the order amf tries agents in for an issue without a
// pipeline when neither --agent nor a usable primary or secondary agent
// names one (see agentPlan).
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

// Defaults for AMF_PRIMARY_AGENT and AMF_SECONDARY_AGENT.
const (
	defaultPrimaryAgent   = agentCodex
	defaultSecondaryAgent = agentClaude
)

// agentNone is the AMF_SECONDARY_AGENT value that turns the fallback off.
const agentNone = "none"

// parseAgentSetting reads AMF_PRIMARY_AGENT or AMF_SECONDARY_AGENT: the
// values --agent takes, and for the secondary also none, which is "".
func parseAgentSetting(key, v string, allowNone bool) (string, error) {
	s := strings.ToLower(strings.TrimSpace(v))
	if allowNone && s == agentNone {
		return "", nil
	}
	if s != "" {
		if a, err := parseAgent(s); err == nil {
			return a, nil
		}
	}
	want := "claudecode, codex or opencode"
	if allowNone {
		want = "claudecode, codex, opencode or none"
	}
	return "", fmt.Errorf("%s must be %s, got %q", key, want, v)
}

// agents reads AMF_PRIMARY_AGENT and AMF_SECONDARY_AGENT. secondary is ""
// for none, and also when it names the primary.
func (cfg Config) agents() (primary, secondary string, err error) {
	if primary, err = parseAgentSetting("AMF_PRIMARY_AGENT", cfg.PrimaryAgent, false); err != nil {
		return "", "", err
	}
	if secondary, err = parseAgentSetting("AMF_SECONDARY_AGENT", cfg.SecondaryAgent, true); err != nil {
		return "", "", err
	}
	if secondary == primary {
		secondary = ""
	}
	return primary, secondary, nil
}

// agentName is the command an agent runs as, for messages: claude, codex
// or opencode.
func agentName(agent string) string {
	if name := agentCommand[agent]; name != "" {
		return name
	}
	return "claude"
}

// agentNameOrNone is agentName, or none for "".
func agentNameOrNone(agent string) string {
	if agent == "" {
		return agentNone
	}
	return agentName(agent)
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

// agentStatus checks one agent.
func (r *Runner) agentStatus(a string) agentStatus {
	bin := r.agentBin(a)
	st := agentStatus{Agent: a, Bin: bin}
	if bin == "" {
		st.Bin = agentCommand[a]
	} else if path, err := exec.LookPath(bin); err == nil {
		st.Path = path
	}
	st.Key, st.Keyed = r.agentKey(a)
	st.Model = r.agentModel(a, "")
	return st
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
		out = append(out, r.agentStatus(a))
	}
	return out
}

// defaultAgent is the first agent amf runs for an issue without a pipeline
// (see agentPlan).
func (r *Runner) defaultAgent() (string, error) {
	plan, err := r.agentPlan()
	if err != nil {
		return "", err
	}
	return plan.Agents[0], nil
}

// agentSkip is a primary or secondary agent amf passed over before running
// anything, because it isn't usable.
type agentSkip struct{ Agent, Reason string }

// runPlan is the agents amf tries, in order, for an issue without a
// pipeline, and the configured ones it passed over as not usable.
type runPlan struct {
	Agents  []string
	Skipped []agentSkip
	// NoFallback says why the plan has a single agent ("" with two), for
	// the failure comment: --agent, no secondary, or an unusable one.
	NoFallback string
}

// describe is the plan for messages: "codex", or "codex, then claude if
// it fails".
func (p runPlan) describe() string {
	names := make([]string, len(p.Agents))
	for i, a := range p.Agents {
		names[i] = agentName(a)
	}
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names, ", then ") + " if it fails"
}

// agentPlan picks the agents for an issue without a pipeline:
//   - with --agent, that one alone, which must be installed, and no fallback;
//   - else the primary agent, then the secondary one, each only when usable
//     (installed, with a key when it needs one: see agentKeys);
//   - else, with neither usable (or none configured, as in tests), the first
//     usable of claude, codex and opencode, as pickAgent chooses it.
func (r *Runner) agentPlan() (runPlan, error) {
	if r.Agent != "" || r.Primary == "" {
		agent, _, err := r.pickAgent()
		if err != nil {
			return runPlan{}, err
		}
		why := "no AMF_PRIMARY_AGENT is configured, so amf picked a single agent"
		if r.Agent != "" {
			why = fmt.Sprintf("--agent %s names a single agent, so there is no fallback", agentName(r.Agent))
		}
		return runPlan{Agents: []string{agent}, NoFallback: why}, nil
	}
	var plan runPlan
	for _, a := range []string{r.Primary, r.Secondary} {
		if a == "" || slices.Contains(plan.Agents, a) {
			continue
		}
		if st := r.agentStatus(a); st.usable() {
			plan.Agents = append(plan.Agents, a)
		} else if len(plan.Agents) == 0 {
			plan.Skipped = append(plan.Skipped, agentSkip{a, st.reason()})
		} else {
			// A secondary that isn't usable used to be dropped without a
			// word, so a failed primary looked as if the fallback never
			// existed. Say why it won't run.
			plan.NoFallback = fmt.Sprintf("the secondary agent %s is not usable: %s", agentName(a), st.reason())
		}
	}
	if len(plan.Agents) > 0 {
		if len(plan.Agents) == 1 && plan.NoFallback == "" {
			switch {
			case len(plan.Skipped) > 0:
				plan.NoFallback = fmt.Sprintf("%s was already the fallback: %s is not usable (%s)", agentName(plan.Agents[0]), agentName(plan.Skipped[0].Agent), plan.Skipped[0].Reason)
			default:
				plan.NoFallback = "AMF_SECONDARY_AGENT is none, or the same as the primary agent"
			}
		}
		return plan, nil
	}
	agent, _, err := r.pickAgent()
	if err != nil {
		return runPlan{}, err
	}
	plan.Agents = []string{agent}
	plan.NoFallback = "neither the primary nor the secondary agent is usable, so amf picked " + agentName(agent)
	return plan, nil
}

// pickAgent picks one agent for an issue without a pipeline: the one
// --agent named, which must be installed, or else the first of claude,
// codex and opencode that is installed and has a key (see agentKeys). It
// also says why each agent before the chosen
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
