# Agent instructions

## Keep SPECS.md current

`SPECS.md` is the single source of truth for amf's behavior. A feature is
finished only when `SPECS.md` describes it, so update it in the same change
as the code whenever that change alters something a user can observe:

- a command, flag, default, output message or exit code
- a configuration variable, or how `.env` is read or written
- the issue lifecycle or the labels
- any step of processing an issue: preflight, checkout, prompt, Claude's
  environment, commit, push, PR or failure handling
- the files amf creates, or the security model

A refactor, a test, or a fix that leaves all of the above as it was needs no
`SPECS.md` edit.

**Done when:** every statement in each section you touched matches the code,
checked against the source rather than from memory, and the change includes
`SPECS.md`.
