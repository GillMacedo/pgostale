# Examples

All data here is real output from Go 1.27.3 on linux/arm64, unless noted.

## ledger

`ledger/` is a made-up service that generates synthetic transactions, parses them and applies them to a book of accounts. Names and amounts are generated.

- `ledger/v1` is the version the profiles were recorded from.
- `ledger/v2` makes typical refactors: renames `parse.ParseLine` to `parse.Line`, adds a guard at the top of `(*Book).Apply`, moves `Digest` from package main to package parse, and adds a closure before the hot one in `Run`.
- `ledger/comments` only adds comments, moves `Total` and `Map` to the top of `book.go`, and moves `normalize` to its own file. No line moves inside a function, so the profile still matches.

## profiles

- `profiles/ledger-v1.pprof`: `go build` of `ledger/v1`, then `ledger -seconds 10 -cpuprofile ...` (10 seconds).
- `profiles/ledger-v1-test.pprof`: `go test -run '^$' -bench Run -benchtime 5s -cpuprofile ...` in `ledger/v1`.

`make profiles` records new ones. They differ on every run.

## logs

`logs/pgo-decisions-v1.txt` and `logs/pgo-decisions-v2.txt` are the compiler's PGO decisions when each version is built with `ledger-v1.pprof`:

    go build -a -pgo=../../profiles/ledger-v1.pprof -gcflags='example.com/ledger/...=-m=2 -d=pgodebug=1' -o /dev/null . 2>&1 | grep -E 'hot-budget check allows|PGO devirtualizing' | sort -u

v1 gets 11 hot-budget inlining decisions and 2 PGO devirtualizations. v2 gets 3 hot-budget inlining decisions and no PGO devirtualization.

## real

The Go compiler commits its own PGO profile at `src/cmd/compile/default.pgo`. Both files were made with `pgostale check --limit 10 .` in `$(go env GOROOT)/src/cmd/compile` of Go 1.27.3:

- `real/cmd-compile-go1.27.3-own-profile.txt`: with the profile Go 1.27.3 ships.
- `real/cmd-compile-go1.27.3-with-go1.22.12-profile.txt`: with `--profile` set to the `default.pgo` from the Go 1.22.12 source tree, downloaded by `GOTOOLCHAIN=go1.22.12`.

Go is BSD-3-Clause licensed. The reports contain function names and line numbers from its source.

### gojq

`real/gojq/` uses [gojq](https://github.com/itchyny/gojq) (MIT license), a jq implementation in Go:

- `gojq-v0.12.17.pprof`: a 10-second CPU profile of gojq v0.12.17 (commit `f4c2cfe`), recorded by `gojqprof.go.txt`, which was copied to `cmd/gojqprof/main.go` in the checkout. It runs `cli.Run` in a loop on 20,000 generated JSON objects with the filter `[.[] | select(.n % 3 == 0) | {k: .name, v: (.tags | join(",")), s: (.nested.b | add)}] | group_by(.s) | map({s: .[0].s, c: length}) | sort_by(.c)`.
- `v0.12.17-with-v0.12.17-profile.txt`: `pgostale check --profile gojq-v0.12.17.pprof --limit 12 ./cmd/gojqprof` on the same commit. Everything matches.
- `v0.12.20-with-v0.12.17-profile.txt`: the same command on v0.12.20 (commit `e7c14ec`), with the driver copied in again. The findings shown were checked by hand: `normalizeNumber` became `parseNumber` with the same body, `binopTypeSwitch` became generic, and `(*env).Next` starts one line lower, so the profile's offset for `env.pushfork(pc)` now lands on `env.push(xs[1:])`.
