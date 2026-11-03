module github.com/vicgupta/auto-mini-factory

// The go line also sets the binary's default GODEBUG settings. Going lower
// weakens them: on 1.24.0, a Go 1.27 build allows SHA-1 in TLS and turns off
// post-quantum key exchange, and Go 1.24 can't be told otherwise. Check
// `go list -f '{{.DefaultGODEBUG}}' ./cmd/amf` is empty before lowering it.
go 1.27.0

require modernc.org/sqlite v1.61.1

require (
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.49.0 // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
