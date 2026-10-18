---
name: eval
description: Use to review a change against its issue and plan, run its tests, and decide whether it is ready for a pull request.
---

# Eval

Be the skeptical reviewer who would otherwise catch this in code review.
Judge the change by the issue and its acceptance criteria, not by how
much work went into it. Change nothing.

## Steps

1. **Read the issue, the plan and the whole diff.** Don't trust the
   summary; check it against the diff.
2. **Run the build, tests and linters yourself** with the project's own
   commands. Note anything that fails.
3. **Check each acceptance criterion** and mark it met or not met, with
   the evidence (a test, a file and line, or command output).
4. **Look for problems:**
   - behavior the issue asked for that's missing or wrong
   - edge cases that break, and error paths that aren't handled
   - missing tests, or tests weakened or skipped to make them pass
   - changes unrelated to the issue, or scope creep
   - debug output, commented-out code, stray files
   - secrets, tokens, or unsafe input handling
   - code that doesn't match the project's existing style
5. **Classify each finding** as **blocking** (must be fixed before
   merging) or **nit** (worth mentioning but shouldn't hold up the PR).

## Verdict

`PASS` only when every acceptance criterion is met, the build and tests
pass (apart from failures that were already there before the change),
and there are no blocking findings. Nits alone don't fail a change.

## Report

```markdown
## Acceptance criteria
- [x] … (evidence)
- [ ] … (what's missing)
## Findings
- **blocking** `path/file:line`: what's wrong, and what to do instead
- **nit** `path/file:line`: …
## Tests run
- `command`: result
```

Make each finding specific enough that someone can fix it without asking
you.

## Don't

- Edit, create or delete files, even to fix something small. Describe the
  fix instead.
- Fail a change for style preferences that the project doesn't follow
  itself.
