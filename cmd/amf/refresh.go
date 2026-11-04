package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// refreshStale handles a retry of an issue whose pull request is still
// open but behind its base branch. Rather than rebuild the change from
// scratch, amf merges origin/base into the PR's branch:
//
//   - a clean merge is pushed (no force) and the PR is done, with no agent;
//   - a merge with conflicts goes to one agent, which is told only which
//     files conflict; its resolution is committed as the merge and pushed;
//   - with conflicts and no usable agent, amf comments on the PR naming the
//     conflicted files and fails, rather than exiting silently.
//
// handled is false when there is no open PR or it is up to date, and the
// issue is processed as usual.
func (r *Runner) refreshStale(ctx context.Context, issue Issue, base string, p *Pipeline, plan runPlan) (pr PullRequest, handled bool, err error) {
	n := issue.Number
	branch, wt := issueBranch(n), r.worktreePath(n)
	open, ok, err := r.GH.OpenPR(ctx, branch)
	if err != nil || !ok {
		return PullRequest{}, false, nil
	}
	_, behind, err := r.GH.Compare(ctx, base, branch)
	if err != nil || behind == 0 {
		return PullRequest{}, false, nil
	}
	r.event(slog.LevelInfo, "issue.refresh", n, fmt.Sprintf("#%d: %s is %d commit(s) behind %s; merging origin/%s into it", n, open.HTMLURL, behind, base, base),
		"number", open.Number, "behind", behind, "base", base)

	if _, err := git(ctx, r.Root, "fetch", "origin", base, branch); err != nil {
		return PullRequest{}, true, err
	}
	if err := r.prepareWorktree(ctx, wt, branch, base); err != nil {
		return PullRequest{}, true, err
	}
	if _, err := git(ctx, wt, "reset", "-q", "--hard", "origin/"+branch); err != nil {
		return PullRequest{}, true, err
	}
	msg := fmt.Sprintf("Merge origin/%s into %s", base, branch)
	if _, merr := git(ctx, wt, "merge", "--no-ff", "-m", msg, "origin/"+base); merr != nil {
		conflicts, cerr := conflictedFiles(ctx, wt)
		if cerr != nil || len(conflicts) == 0 {
			git(ctx, wt, "merge", "--abort")
			return PullRequest{}, true, fmt.Errorf("merging origin/%s into %s: %w", base, branch, merr)
		}
		if err := r.resolveConflicts(ctx, issue, open, base, conflicts, p, plan); err != nil {
			git(ctx, wt, "merge", "--abort")
			return PullRequest{}, true, err
		}
		if _, err := git(ctx, wt, "commit", "--no-edit", "-m", msg); err != nil {
			return PullRequest{}, true, err
		}
	}
	changed, err := changedPaths(ctx, wt, base, "HEAD")
	if err != nil {
		return PullRequest{}, true, err
	}
	if err := pathRefusal(changed); err != nil {
		return PullRequest{}, true, err
	}
	r.event(slog.LevelInfo, "issue.push", n, fmt.Sprintf("#%d: pushing %s", n, branch), "branch", branch)
	// No --force: the merge only adds to the branch.
	if _, err := git(ctx, wt, "push", "-u", "origin", branch); err != nil {
		return PullRequest{}, true, err
	}
	head, err := git(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return PullRequest{}, true, err
	}
	open.Head = head
	if _, err := git(ctx, r.Root, "worktree", "remove", "--force", wt); err != nil {
		r.event(slog.LevelWarn, "issue.cleanup", n, fmt.Sprintf("#%d: could not remove worktree: %v", n, err), "error", err)
	}
	return open, true, nil
}

// conflictedFiles lists the paths a merge in progress left unmerged.
func conflictedFiles(ctx context.Context, wt string) ([]string, error) {
	out, err := git(ctx, wt, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// conflictAgent is the agent that resolves merge conflicts: the pipeline's
// code stage, else the first agent of the plan, if it is usable.
func (r *Runner) conflictAgent(p *Pipeline, plan runPlan) (agent, model string, ok bool) {
	switch {
	case p != nil && p.Code != nil:
		agent, model = p.Code.Agent, p.Code.Model
	case len(plan.Agents) > 0:
		agent = plan.Agents[0]
	default:
		return "", "", false
	}
	return agent, r.agentModel(agent, model), r.agentStatus(agent).usable()
}

// resolveConflicts has one agent resolve conflicts in the issue's worktree
// and stages the result. Without a usable agent it comments on the PR.
func (r *Runner) resolveConflicts(ctx context.Context, issue Issue, pr PullRequest, base string, files []string, p *Pipeline, plan runPlan) error {
	n := issue.Number
	branch, wt := issueBranch(n), r.worktreePath(n)
	agent, model, ok := r.conflictAgent(p, plan)
	if !ok {
		r.comment(ctx, pr.Number, fmt.Sprintf("amf could not bring this pull request up to date: merging `%s` into `%s` conflicts in:\n\n%s\n\nNo usable agent is available to resolve them, so the branch is unchanged. Merge `%s` into it and resolve the conflicts by hand.",
			base, branch, bulletList(files), base))
		return fmt.Errorf("merging origin/%s into %s conflicts in %s, and no usable agent can resolve it", base, branch, strings.Join(files, ", "))
	}
	r.lastAgent = agentName(agent)
	r.event(slog.LevelInfo, "issue.agent", n, fmt.Sprintf("#%d: %s resolving conflicts in %s", n, agentName(agent), strings.Join(files, ", ")),
		"agent", agentName(agent), "model", model, "files", files)
	log := filepath.Join(r.Root, ".amf", "logs", fmt.Sprintf("issue-%d-conflicts.log", n))
	out := filepath.Join(wt, ".amf", "code.md")
	if _, err := r.runAgent(ctx, agent, "", model, wt, conflictPrompt(r.Repo, branch, base, issue, files), log, out); err != nil {
		return err
	}
	os.RemoveAll(filepath.Join(wt, ".amf"))
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(wt, f))
		if err == nil && hasConflictMarkers(string(data)) {
			return fmt.Errorf("%s left conflict markers in %s", agentName(agent), f)
		}
	}
	if _, err := git(ctx, wt, "add", "-A", "--", "."); err != nil {
		return err
	}
	if left, err := conflictedFiles(ctx, wt); err != nil || len(left) > 0 {
		return fmt.Errorf("%s left conflicts unresolved in %s", agentName(agent), strings.Join(left, ", "))
	}
	return nil
}

func hasConflictMarkers(s string) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "<<<<<<< ") || strings.HasPrefix(l, ">>>>>>> ") {
			return true
		}
	}
	return false
}

func bulletList(items []string) string {
	var b strings.Builder
	for _, s := range items {
		fmt.Fprintf(&b, "- `%s`\n", s)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func conflictPrompt(repo, branch, base string, issue Issue, files []string) string {
	return fmt.Sprintf(`You are working in a git worktree of the GitHub repository %s, on branch %s,
which resolves issue #%d: %s

amf is merging origin/%s into this branch, and the merge conflicts in only these files:

%s

Instructions:
- Resolve the conflicts in those files only, keeping both the branch's change and %s's changes.
- Remove every conflict marker. Do not change any other file.
- Run the project's build and tests if you can, and fix failures the merge caused.
- Do not create, edit or delete anything under .github/.
- Do not commit, push, abort the merge or create branches. amf commits the merge.
- When you are done, reply with a short note of how you resolved each file.
`, repo, branch, issue.Number, issue.Title, base, bulletList(files), base)
}
