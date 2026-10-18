---
name: plan
description: Use to turn a GitHub issue into a concrete implementation plan before any code is written.
---

# Plan

Turn the issue into a plan that another engineer could follow without
asking questions. Read the code before you write. Change nothing.

## Steps

1. **Restate the goal** in one or two sentences, in terms of what a user
   or caller will notice.
2. **Write acceptance criteria:** a short checklist that a reviewer can
   check off one by one. Use the issue's own criteria if it has any. Where
   you infer one, mark it _(assumed)_.
3. **Explore.** Find the code involved, the patterns nearby that the
   change should copy, and the tests that cover it. Find the project's
   build, test and lint commands in `AGENTS.md`, `CLAUDE.md`, the README,
   the Makefile or the CI config.
4. **Choose the smallest complete change.** Prefer extending what exists
   over adding new structure. Say what you're deliberately leaving out.
5. **Write the plan** in this format:

   ```markdown
   ## Goal
   ## Acceptance criteria
   - [ ] …
   ## Changes
   - `path/to/file`: what changes and why
   ## Tests
   - New or updated tests, and the exact commands to run
   ## Out of scope
   ## Risks
   ```

## When to stop

If the issue is ambiguous in a way that changes the result, or needs
access you don't have (secrets, external services, a product decision),
don't guess. Start your reply with `BLOCKED:` and list specific
questions, each answerable in a sentence.

## Don't

- Edit, create or delete files.
- Plan refactors, renames or cleanups the issue doesn't need.
- Add dependencies unless there's no reasonable alternative; if you do,
  say why.
