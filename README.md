# amf — autonomous mini factory

One pure-Go binary (no cgo) that turns GitHub issues into pull requests with a
coding agent (Claude Code, Codex or opencode, whichever you have), or with a
plan → code → eval pipeline of them. For a solo developer or small team that wants coding agents without
running a platform.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/vicgupta/auto-mini-factory/main/install.sh | sh
```

This downloads the latest release binary for macOS or Linux (amd64/arm64),
checks it against the release's `checksums.txt`, and installs it to
`/usr/local/bin` if that's writable, otherwise `~/.local/bin`. To choose,
set `AMF_VERSION=v0.0.2` or `AMF_INSTALL_DIR=...` on the `sh` side of the
pipe, e.g. `... | AMF_INSTALL_DIR=~/bin sh`.

On Windows, download `amf_<version>_windows_amd64.zip` (or `_arm64`) from the
[latest release](https://github.com/vicgupta/auto-mini-factory/releases/latest),
unzip it, and put `amf.exe` in a folder on your `PATH`. After that,
`amf update` keeps it current. The Windows build is cross-compiled and its
self-update is unit-tested, but it hasn't been run on a Windows machine yet.

Or with Go 1.27 or newer:

```sh
go install github.com/vicgupta/auto-mini-factory/cmd/amf@latest
# or, from a clone: go build -o amf ./cmd/amf
```

Needs `git` and at least one coding agent on `PATH`:
[`claude`](https://claude.com/claude-code), [`codex`](https://github.com/openai/codex)
or [`opencode`](https://opencode.ai). See [Choosing the agent](docs/COMMANDS.md#choosing-the-agent)
for which one amf runs.

To update later:

```sh
amf update            # replace this amf with the latest release
amf update --check    # only say whether there is a newer one
amf update --version v0.0.7   # install a specific release (also downgrades)
```

`amf update` downloads the same release archives as `install.sh`, checks them
against `checksums.txt`, makes sure the new binary runs, then swaps it in place
of the running one. It needs write access to the folder `amf` is in.

## Quick start

Have a git identity and credentials that can push to your repo. See
[Choosing the agent](docs/COMMANDS.md#choosing-the-agent) for agent key and
login requirements.

```sh
cd your-repo             # a clone of the GitHub repo you want to work on
amf init                 # writes .env; fills GITHUB_REPO from origin
# Edit .env: set GITHUB_TOKEN (or use gh / amf auth login), check GITHUB_REPO and configure your agent.
amf init                 # creates any missing amf labels once configured
amf doctor               # check the setup
amf issue "Add dark mode" --body "Use CSS variables for the palette"
amf run 42               # replace 42 with the issue number in the URL printed above
```

Or run `amf watch` to process open issues labeled `amf:ready`, oldest first.

For all settings and defaults, run `amf help env` or see the
[configuration guide](docs/CONFIGURATION.md).

## Documentation

amf never invokes a pager. Long output from `amf pr diff` and `amf memory show`
goes directly to stdout, even in a terminal; there is nothing to disable.
There is no `--no-pager` flag, and `PAGER` (including `PAGER=cat`) has no effect
on amf's output. To page it yourself, pipe to a pager:

```sh
amf pr diff 42 | less
amf memory show 42 | less
```

`amf help <command>` prints a command's details. See:

- [Command reference](docs/COMMANDS.md): commands, flags, exit status, paths and logs
- [Man page](docs/COMMANDS.md#man-page): generate and install with `amf man`
- [Configuration](docs/CONFIGURATION.md): `.env`, authentication and `amf doctor`
- [Examples cookbook](docs/EXAMPLES.md): complete workflows
- [Troubleshooting](docs/TROUBLESHOOTING.md): common failures and fixes
- [Specification](docs/SPECS.md) and [issue processing](docs/AMF-PROCESS.md)
- [Development](docs/DEVELOPMENT.md): offline and live tests
- [Release notes](https://github.com/vicgupta/auto-mini-factory/releases): changes by version

## Security

Agents run with all permission checks off, so an issue body is effectively
code you're executing on your machine. Only label issues you trust; set
`AMF_TRUSTED` to limit issue authors. Agents can read local credentials even
though amf strips GitHub tokens from their environment. See the
[security model](docs/SPECS.md#security-model) before running amf or enabling
`--merge`.

## License

MIT; see [LICENSE](LICENSE).
