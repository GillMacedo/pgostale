---
name: checking-go-pgo-profiles
description: Checks whether a Go PGO profile (default.pgo) still matches the code it is applied to, by building the main package with the profile and reading which hot call sites the compiler still matches. Explains each lost function or call site, such as a rename, a move to another package, inserted lines or a renumbered closure, and says whether to collect a new profile. Use when the user asks if their default.pgo is stale or still works, when to refresh a PGO profile, why PGO gains dropped after a refactor, whether a profile from go test or another service can be used, or wants a review of a change to a Go program that ships a default.pgo.
---

# Checking Go PGO profiles

Go's profile-guided optimization finds hot call sites by function name and by line offset from the start of the function. After a rename, a move to another package, or a few lines added above a call, those call sites stop matching, and `go build` says nothing. The bundled tool builds the package with the profile, asks the compiler which call sites it matched, and reports what is left. It needs Go and compiles code, but never runs it.

## Run the tool

Find the main packages that have a profile with `find . -name default.pgo -not -path './vendor/*'`. Then, from inside the module, check each one:

    go run "${CLAUDE_PLUGIN_ROOT}/scripts/pgostale.go" check ./cmd/server

- The profile defaults to `default.pgo` in the package directory. Use `--profile FILE` for a profile kept elsewhere, such as one passed with `go build -pgo=FILE`.
- Add `--limit 0` to see every lost edge, or `--json` for the full result.
- To check a change in review, run it on the working tree. To compare with the base branch, run it on a checkout of the base as well. A profile that matched on the base and doesn't on the change was broken by the change.
- Exit code 2 means the tool couldn't check: say why, from its message, and don't guess at a result. Common causes are a cut-off or empty profile, a heap profile instead of a CPU profile, and a package that doesn't build.

## Read the report

The headline is the share of the module's hot profile weight the compiler still matches. Hot means the edges the compiler considers for PGO inlining: the heaviest ones that make up 99% of the profile weight. Below 90% (`--min`) the report says the profile is stale.

For the biggest losses, in this order:

1. **Functions this build doesn't have.** Read the hint. "Moved to another package", "receiver changed", "probably renamed to" and "a closure" each say what happened in the code. Confirm it with `git log -S'FuncName'` or by reading the new function. "The profile names package main by its import path" means the profile came from `go test -cpuprofile` and was never valid for the binary.
2. **Call sites that moved.** The function still exists, but the call the profile points at is now on another line, usually because lines were added above it. Show the user the line and the call that moved.
3. **"calls X now".** The compiler still treats that line as a hot call site, but the call there is a different one now, so PGO applies the profile's weight to the wrong call.

## Report

1. Give the match share and the number of hot edges lost, and say whether the profile needs refreshing.
2. List the largest losses with `file:line` and the cause in one sentence each.
3. Recommend collecting a new CPU profile from the current build running a representative load, as in the Go PGO guide: `curl -o default.pgo 'http://HOST/debug/pprof/profile?seconds=30'` from a service with `net/http/pprof`, or merging several with `go tool pprof -proto a.pprof b.pprof > default.pgo`. A profile from `go test` benchmarks is only fine when the hot code is outside package main.

## Don't

- Don't edit `default.pgo` or try to fix it by hand.
- Don't claim a slowdown or speedup from this report. It shows what the compiler can match, not how much time it saves. Suggest a benchmark with and without `-pgo=off` to measure.
- Don't call a profile fine because the report says 100%. Only call edges whose caller is in the module are checked, not the standard library or dependencies, and a profile can match and still not be representative of production load.
