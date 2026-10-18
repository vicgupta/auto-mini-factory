---
name: code
description: Use to implement a planned change to resolve a GitHub issue, including tests, and to fix findings from a review.
---

# Code

Implement the plan. Make the change look as if the project's own
maintainers wrote it.

## Steps

1. **Follow the plan.** If the plan turns out to be wrong, deviate as
   little as possible and explain why in your summary.
2. **Match the surrounding code:** naming, error handling, comment
   density, file layout. Copy the nearest existing example of the same
   kind of change.
3. **Test the behavior.** Add or update tests for what the issue changes,
   including the edge case most likely to break. Don't weaken, skip or
   delete existing tests to make them pass.
4. **Verify.** Run the build, the tests and the linters with the
   project's own commands. Fix every failure your change caused. If a
   failure was already there before your change, leave it and mention it.
5. **Review your diff** before finishing. Remove debug output, commented-out
   code, stray files and unrelated edits.

## Fixing review findings

When you're given findings from a review, your previous attempt is
already in the working tree. Fix every blocking finding. If you disagree
with one, leave the code as it is and explain why in your summary. Don't
start over unless the findings say the approach is wrong.

## Summary

Reply with a pull request description:

```markdown
## What
## Why
## How I tested it
## Notes for the reviewer
```

The last section covers deviations from the plan, failures that were
already there, and anything you were unsure about.

## Don't

- Touch git (commit, branch, push) or GitHub. amf does that.
- Change CI config, `.claude/`, `AGENTS.md` or `CLAUDE.md` unless the
  issue asks for it.
- Add dependencies that the plan doesn't call for.
- Put secrets, tokens or personal data in code, tests or the summary.
