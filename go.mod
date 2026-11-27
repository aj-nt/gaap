module github.com/aj-nt/gaap

go 1.27.2

require (
	github.com/aj-nt/gaap/platform v0.16.0
	github.com/aj-nt/vassago-sdk v0.5.0
	github.com/google/uuid v1.6.0
	golang.org/x/sys v0.49.0
	google.golang.org/grpc v1.83.1
	modernc.org/sqlite v1.62.0
)

// In-tree development of the kernel uses go.work (see WORKSPACE NOTE at the bottom);
// nothing in this file may redirect a module, because `go install pkg@version`
// refuses any module whose go.mod carries a replace directive. That rule is why the
// broker could not be installed from its own tag.
require (
	github.com/aj-nt/gaap/protocol v0.1.0 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	modernc.org/libc v1.80.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

// WORKSPACE NOTE: `go.work` maps platform/ and protocol/ to this checkout for local
// work and for CI's default run. A consumer never sees it — a downloaded module's
// go.work is ignored — so the published artefact builds against the published tags.
// CI runs `GOWORK=off` as well, so drift between the tree and the tags fails there
// rather than at a user's `go get`.
