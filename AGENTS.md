# Agent instructions

## Keep SPECS.md current

`SPECS.md` is the single source of truth for amf's behavior. A feature is
finished only when `SPECS.md` describes it, so update it in the same change
as the code whenever that change alters something a user can observe:

- a command, flag, default, output message or exit code
- a configuration variable, or how `.env` is read or written
- the issue lifecycle or the labels
- the `## amf` pipeline table, a stage, an agent's command or how its final
  message is read
- any step of processing an issue: preflight, checkout, prompt, an agent's
  environment, commit, push, PR or failure handling
- the files amf creates, or the security model

A refactor, a test, or a fix that leaves all of the above as it was needs no
`SPECS.md` edit.

**Done when:** every statement in each section you touched matches the code,
checked against the source rather than from memory, and the change includes
`SPECS.md`.

## Releasing

Releases are for the maintainer (the repo owner) only; end users never run
this and get new versions with `amf update`. Publish only with
`scripts/release.sh`, which refuses unless `gh` is logged in as the owner;
never build or upload archives by hand. `install.sh` and `amf update` find
the binary at `amf_<tag>_<os>_<arch>/amf` inside each archive, and an archive
without that folder breaks both (this happened to v0.1.1 and was fixed by
re-uploading).

1. **Pick the version.** Bump the patch (`v0.1.1` → `v0.1.2`) for fixes,
   docs and packaging, and the minor (`v0.1.x` → `v0.2.0`) for new commands,
   flags or behavior. Tags are `vX.Y.Z`.
2. **Write the notes** in a file outside the repo, so they never get
   committed:
   - `## Changes`: what a user notices, one bullet each, bold lead-in first.
   - `## Upgrade`: `amf update`, plus the `install.sh` one-liner when older
     versions can't self-update.
   - `**Full changelog:**` linking `compare/<previous tag>...<tag>`.
   - No Claude attribution ("Generated with Claude Code", `Co-Authored-By`);
     the script refuses notes that contain it.
3. **Dry run:** `scripts/release.sh --dry-run vX.Y.Z` runs vet and tests,
   builds every platform, and checks the archives without publishing.
4. **Release:** `scripts/release.sh vX.Y.Z notes.md`. From a clean `main`
   that matches `origin/main`, it builds `darwin` and `linux` for `arm64` and
   `amd64`, writes `checksums.txt`, runs this machine's binary to check its
   version, creates the tag and the release titled `amf vX.Y.Z` on the
   current commit, and checks that GitHub serves the archive it uploaded.
5. **Verify:** `amf update --check` should name the new version, as
   available or up to date.

If you replace a release's files, GitHub's download cache can serve the old
ones for a few minutes, so a failure right after a re-upload may be stale.
