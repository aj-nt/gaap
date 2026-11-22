module github.com/aj-nt/gaap

go 1.27.2

require (
	github.com/aj-nt/gaap/platform v0.8.0
	github.com/aj-nt/vassago-sdk v0.5.0
	github.com/google/uuid v1.6.0
	golang.org/x/sys v0.49.0
	google.golang.org/grpc v1.83.1
	modernc.org/sqlite v1.62.0
)

// The kernel is developed in-tree and released on its own cadence. Consumers see
// the tag (`platform/vX.Y.Z`); this replace is for local work and CI only, and is
// ignored by anyone who depends on gaap.
replace github.com/aj-nt/gaap/platform => ./platform

require (
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
