# Developing amf

Back to the [README](../README.md).

```sh
go test -race ./...
```

CI runs `gofmt`, `go vet` and these tests on Linux and macOS for every push
and PR, and builds every release platform.

The documentation tests compare command, help topic and configuration variable
names in the built-in help with the command reference and configuration guide
tables, ignoring wording, whitespace and ordering.

Tests use `httptest` for GitHub, real git repos in temp dirs, and shell
scripts standing in for `claude`, `codex` and `opencode`.

The live tests run the real binary and agents against a real repository. They
create issues, branches and PRs there and close or delete them afterwards:

```sh
AMF_LIVE_DIR=/path/with/.env go test -tags live -run TestLive -v -timeout 90m ./cmd/amf
```

`AMF_LIVE_DIR` holds a `.env` for a test repository and is where amf runs.
`AMF_LIVE_PIPELINE=1` also tests pipelines, which commits an `AGENTS.md` to
that repository's default branch and removes it afterwards. See
[`cmd/amf/live_test.go`](../cmd/amf/live_test.go) for the other settings.

