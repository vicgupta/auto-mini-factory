module github.com/vicgupta/auto-mini-factory

// The go line also sets the binary's default GODEBUG settings. Going lower
// weakens them: on 1.24.0, a Go 1.27 build allows SHA-1 in TLS and turns off
// post-quantum key exchange, and Go 1.24 can't be told otherwise. Check
// `go list -f '{{.DefaultGODEBUG}}' ./cmd/amf` is empty before lowering it.
go 1.27.0
