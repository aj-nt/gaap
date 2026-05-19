# Contributing to Gaap

## Architecture

Gaap is the coordination layer. It decomposes goals into task DAGs, declares them on Vassago (shared memory), observes results, and advances the graph. It has zero storage concern -- all persistent state lives in Vassago.

```
Goal -> [Gaap: decompose -> DAG -> declare tasks] -> Vassago <- [agents: discover, claim, execute, publish]
                                                                          ^
                    [Gaap: observe results -> advance DAG -> synthesize] --^
```

## Development

### Prerequisites

- Go 1.25+
- A running Vassago daemon (tested against vassago-sdk v0.3.0)

### Build

```bash
go build ./...
```

### Test

```bash
go test -race ./...
```

### CI

CI runs on every push to main and every PR: `go test -race`, `go vet`, `go build`. Release workflow triggers on `v*` tags.

## Code Style

- Standard Go conventions
- Tests use the standard library `testing` package
- Integration tests require a running Vassago daemon on `localhost:50051`

## License

Apache 2.0. All contributions are under the same license.
