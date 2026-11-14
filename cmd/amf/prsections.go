package main

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

const (
	prTestingMissing   = "The agent did not report how it tested this change."
	prDecisionsMissing = "The agent did not report decisions on open questions."
	prReadmeMissing    = "README.md was not changed, so the feature list has no entry for this change."
)

const prSectionsInstruction = `- Your final message must also contain these Markdown sections:
  ## Testing
  The exact commands you ran and their actual results; never invent numbers.
  ## Decisions on open questions
  Each open question in the issue and the cautious choice you made, or "None."
  if the issue has none.
  If the change adds or changes a user-visible feature, add or update its entry
  in README.md's feature list with a link to its docs.
`

type agentSections struct {
	Summary                  string
	Testing, Decisions       string
	HasTesting, HasDecisions bool
}

type markdownHeading struct {
	line, level int
	kind        string
}

func markdownHeadingAt(line string) (level int, name string, ok bool) {
	line = strings.TrimSpace(line)
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	rest := line[level:]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return 0, "", false
	}
	name = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(rest), "#"))
	if name == "" {
		return 0, "", false
	}
	return level, name, true
}

func markdownSectionKind(name string) string {
	name = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(name), ":")))
	switch name {
	case "testing":
		return "testing"
	case "decisions on open questions", "decisions":
		return "decisions"
	case "docs updated":
		return "docs"
	case "summary":
		return "summary"
	}
	return ""
}

func splitAgentSections(text string) agentSections {
	lines := strings.Split(text, "\n")
	var headings []markdownHeading
	inFence := false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if level, name, ok := markdownHeadingAt(t); ok {
			headings = append(headings, markdownHeading{i, level, markdownSectionKind(name)})
		}
	}
	removed := make([]bool, len(lines))
	var out agentSections
	for i, h := range headings {
		if h.kind == "" {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(headings); j++ {
			if headings[j].level <= h.level {
				end = headings[j].line
				break
			}
		}
		switch h.kind {
		case "summary":
			removed[h.line] = true
		case "testing":
			for k := h.line; k < end; k++ {
				removed[k] = true
			}
			if !out.HasTesting {
				if s := strings.TrimSpace(strings.Join(lines[h.line+1:end], "\n")); s != "" {
					out.Testing, out.HasTesting = s, true
				}
			}
		case "decisions":
			for k := h.line; k < end; k++ {
				removed[k] = true
			}
			if !out.HasDecisions {
				if s := strings.TrimSpace(strings.Join(lines[h.line+1:end], "\n")); s != "" {
					out.Decisions, out.HasDecisions = s, true
				}
			}
		case "docs":
			for k := h.line; k < end; k++ {
				removed[k] = true
			}
		}
	}
	var keep []string
	for i, line := range lines {
		if !removed[i] {
			keep = append(keep, line)
		}
	}
	out.Summary = strings.TrimSpace(strings.Join(keep, "\n"))
	return out
}

func docsChanged(files []string) []string {
	var docs []string
	for _, p := range files {
		low := strings.ToLower(p)
		if strings.HasPrefix(low, "docs/") || strings.HasPrefix(low, "doc/") ||
			strings.HasSuffix(low, ".md") || strings.HasSuffix(low, ".mdx") || strings.HasSuffix(low, ".rst") {
			docs = append(docs, p)
		}
	}
	return docs
}

func (r *Runner) sectionedPRBody(summary, extra string, n int, f changeFacts) string {
	// Clean first, so an amf-status or attribution line at the end of the
	// last section never lands in it.
	sec := splitAgentSections(cleanSummary(summary, n))
	summary = sec.Summary
	testing := strings.TrimSpace(sec.Testing)
	decisions := strings.TrimSpace(sec.Decisions)
	if f.Part {
		summary = rewriteClosingKeywords(summary, n)
		extra = rewriteClosingKeywords(extra, n)
		testing = rewriteClosingKeywords(testing, n)
		decisions = rewriteClosingKeywords(decisions, n)
	}
	if summary == "" {
		if f.Part {
			summary = fmt.Sprintf("Part of #%d.", n)
		} else {
			summary = fmt.Sprintf("Resolves #%d.", n)
		}
	}
	if extra != "" {
		summary += "\n\n" + extra
	}
	hasTesting := sec.HasTesting && testing != ""
	hasDecisions := sec.HasDecisions && decisions != ""
	readmeChanged := slices.Contains(f.Files, "README.md")

	var b strings.Builder
	b.WriteString("## Summary\n\n_Agent summary: written by the agent; amf has not verified it._\n\n")
	b.WriteString(summary + "\n\n")
	b.WriteString("## Changes\n\n_What amf observed in git, not the agent._\n\n")
	if f.Diffstat != "" {
		b.WriteString("```\n" + strings.TrimRight(f.Diffstat, "\n") + "\n```\n\n")
	}
	b.WriteString("Changed files:\n")
	for _, path := range f.Files {
		b.WriteString("- `" + path + "`\n")
	}
	b.WriteString("\n## Testing\n\n_Reported by the agent; amf has not verified it._\n\n")
	if hasTesting {
		b.WriteString(testing)
	} else {
		b.WriteString(prTestingMissing)
	}
	b.WriteString("\n\n## Docs updated\n\n_From git, not the agent._\n\n")
	docs := docsChanged(f.Files)
	if len(docs) == 0 {
		b.WriteString("None.\n")
	} else {
		for _, path := range docs {
			b.WriteString("- `" + path + "`\n")
		}
	}
	b.WriteString("\n## Decisions on open questions\n\n_Reported by the agent; amf has not verified it._\n\n")
	if hasDecisions {
		b.WriteString(decisions)
	} else {
		b.WriteString(prDecisionsMissing)
	}
	if !readmeChanged {
		b.WriteString("\n\n## README feature list\n\n" + prReadmeMissing)
	}
	b.WriteString("\n\n")
	if f.Part {
		b.WriteString(fmt.Sprintf("Part of #%d", n))
	} else {
		b.WriteString(fmt.Sprintf("Closes #%d", n))
	}
	b.WriteString("\n\n---\n_Opened by amf._")

	var missing []string
	if !hasTesting {
		missing = append(missing, "testing")
	}
	if !hasDecisions {
		missing = append(missing, "decisions")
	}
	if !readmeChanged {
		missing = append(missing, "readme")
	}
	if len(missing) > 0 && (r.Log != nil || r.Events != nil) {
		r.event(slog.LevelWarn, "pr.sections_missing", n, fmt.Sprintf("#%d: PR sections missing: %s", n, strings.Join(missing, ", ")), "sections", missing)
	}
	return b.String()
}
