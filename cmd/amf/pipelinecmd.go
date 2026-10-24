package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

// cmdPipeline checks the pipeline in the enclosing checkout's AGENTS.md,
// as it is on disk, so a change can be tested before it's merged.
func cmdPipeline(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pipeline", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fetch := fs.Bool("fetch", false, "also download and check each stage's skill")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf pipeline [--fetch]")
		return errUsage
	}
	root, err := git(ctx, ".", "rev-parse", "--show-toplevel")
	if err != nil {
		return errors.New("run amf pipeline inside a git checkout")
	}
	path := filepath.Join(root, "AGENTS.md")
	md, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return noPipeline(ctx, stdout, "No AGENTS.md in "+root)
	}
	if err != nil {
		return err
	}
	p, err := parsePipeline(string(md))
	if err != nil {
		return err
	}
	if p == nil {
		return noPipeline(ctx, stdout, "No ## amf table in "+path)
	}

	// Only the agent paths are needed, so a .env without a token is fine.
	cfg, _ := loadConfig(envPaths(ctx)...)
	r := &Runner{Claude: cfg.Claude, Codex: cfg.Codex, Opencode: cfg.Opencode, OpencodeModel: cfg.OpencodeModel}
	fmt.Fprintf(stdout, "Pipeline in %s:\n\n", path)
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STAGE\tAGENT\tMODEL\tSKILL")
	for _, s := range p.stages() {
		skill := s.Skill
		if skill == "" {
			skill = "-"
		}
		model := s.model()
		if m := r.agentModel(s.Agent, s.Model); m != s.Model {
			model = m + " (OPENCODE_MODEL)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Name, s.Agent, model, skill)
	}
	tw.Flush()
	fmt.Fprintln(stdout)

	problems := 0
	seen := map[string]bool{}
	for _, s := range p.stages() {
		if seen[s.Agent] {
			continue
		}
		seen[s.Agent] = true
		if bin, err := exec.LookPath(r.agentBin(s.Agent)); err != nil {
			fmt.Fprintf(stdout, "agent %-10s  missing: %s is not installed\n", s.Agent, r.agentBin(s.Agent))
			problems++
		} else {
			fmt.Fprintf(stdout, "agent %-10s  ok: %s\n", s.Agent, bin)
		}
	}
	if *fetch {
		for _, s := range p.stages() {
			if s.Skill == "" {
				continue
			}
			text, err := fetchSkill(ctx, nil, s.Skill)
			if err != nil {
				fmt.Fprintf(stdout, "skill %-10s  failed: %v\n", s.Name, err)
				problems++
				continue
			}
			fmt.Fprintf(stdout, "skill %-10s  ok: %.1f KB, sha256 %x\n", s.Name, float64(len(text))/1024, sha256.Sum256([]byte(text)))
		}
	}
	if problems > 0 {
		return fmt.Errorf("the pipeline has %d %s", problems, plural(problems, "problem", "problems"))
	}
	return nil
}

// noPipeline reports which agent amf runs without a pipeline: the first
// usable of claude, codex and opencode (see pickAgent). None usable is an
// error.
func noPipeline(ctx context.Context, stdout io.Writer, why string) error {
	cfg, _ := loadConfig(envPaths(ctx)...)
	r := &Runner{Claude: cfg.Claude, Codex: cfg.Codex, Opencode: cfg.Opencode, ClaudeToken: cfg.ClaudeToken, AnthropicKey: cfg.AnthropicKey, OpenAIKey: cfg.OpenAIKey, OpencodeKey: cfg.OpencodeKey}
	agent, passed, err := r.pickAgent()
	if err != nil {
		fmt.Fprintf(stdout, "%s, so amf runs one agent with its built-in prompt, but %v.\n", why, err)
		return errors.New("the pipeline has 1 problem")
	}
	because := ""
	if len(passed) > 0 {
		because = " (" + strings.Join(passed, "; ") + ")"
	}
	fmt.Fprintf(stdout, "%s, so amf runs %s with its built-in prompt%s, unless --agent names another.\n", why, agentName(agent), because)
	return nil
}
