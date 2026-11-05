# pgostale: check if your Go PGO profile (default.pgo) still matches your code

[![CI](https://github.com/GillMacedo/pgostale/actions/workflows/ci.yml/badge.svg)](https://github.com/GillMacedo/pgostale/actions/workflows/ci.yml) [![MIT license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**A Claude Code skill and Go CLI for Go teams that build with profile-guided optimization. It builds your main package with its `default.pgo`, asks the compiler which hot call sites it still matches, and tells you how much of the profile still applies and what was lost: functions that were renamed or moved, calls that moved to another line, and lines where PGO now boosts the wrong call.**

Go's PGO finds hot call sites by function name and by line offset from the start of the function. Rename a function, move it to another package, or add a guard clause at the top, and the profile no longer matches that code. `go build` doesn't warn about it: the build loses the optimizations the profile gave that code, and nothing says so. `pgostale` reads the compiler's own matching (`-gcflags=-d=pgodebug=3`), so it reports exactly what the compiler can and can't use.

## What you can ask Claude

- "Is our default.pgo still any good after this refactor?"
- "Did my PR break the PGO profile for cmd/server?"
- "Can I use a profile from `go test -bench` as default.pgo?"
- "Why did our PGO gains drop after the last release?"

## Install

As a Claude Code plugin:

    claude plugin marketplace add GillMacedo/pgostale
    claude plugin install pgostale@pgostale-marketplace

Or as a CLI with Go 1.22 or newer. It has no dependencies. From inside your module:

    go run github.com/GillMacedo/pgostale/plugin/scripts@latest check ./cmd/server

## Example

`examples/ledger` holds three versions of a made-up service. The profile `examples/profiles/ledger-v1.pprof` is a real 10-second CPU profile of `v1`. `v2` makes four ordinary refactors: it renames `parse.ParseLine` to `parse.Line`, adds a three-line guard at the top of `(*Book).Apply`, moves `Digest` from package main to package `parse`, and adds a small closure before the hot one in `Run`. From a clone of this repo:

    $ go run ./plugin/scripts check -C examples/ledger/v2 --profile examples/profiles/ledger-v1.pprof
    Profile examples/profiles/ledger-v1.pprof on example.com/ledger
    Profile: 119 call edges, 104 hot, total weight 1565.
    In this module: 30.6% of the weight (17 hot edges). Standard library and other modules: 68.7%, not checked. Calls to compiler-inserted runtime helpers: 0.7%, skipped.
    
    Still matches: 47.6% of this module's hot weight (5 of 17 hot edges), 47.6% of all its weight.
    Below the minimum of 90%: the profile is stale for this code. Collect a new one from the current build.
    
    Functions in the profile that this build doesn't have:
      main.Digest  weight 125 (8.0%), 1 hot edge
          moved to another package: example.com/ledger/parse.Digest
      example.com/ledger/parse.ParseLine  weight 36 (2.3%), 4 hot edges
          probably renamed to example.com/ledger/parse.Line, which makes the same calls at the same lines
      main.Run.func1  weight 30 (1.9%), 2 hot edges
          a closure; the compiler numbers closures by their order in the function, so adding or removing one before it renames it
    
    Call sites that moved or changed in functions that still exist:
      main.Run +12 -> main.Run.func1  weight 39 (2.5%), hot
          no call at examples/ledger/v2/main.go:76 now
      example.com/ledger/book.(*Book).Apply +5 -> example.com/ledger/book.(*Book).account  weight 11 (0.7%), hot
          no call at examples/ledger/v2/book/book.go:61 now
      main.Run +14 -> example.com/ledger/book.Map[go.shape.struct { From string; To string; Cents int64 },go.shape.int64]  weight 8 (0.5%), hot
          no call at examples/ledger/v2/main.go:78 now
      example.com/ledger/book.(*Book).Apply +6 -> example.com/ledger/book.(*Book).account  weight 1 (0.1%), hot
          examples/ledger/v2/book/book.go:62 calls Adjust now; the compiler still treats it as this hot call site
      example.com/ledger/book.(*Book).Apply +3 -> example.com/ledger/book.LimitRule.Adjust  weight 1 (0.1%), hot
          no call at examples/ledger/v2/book/book.go:59 now

None of this shows up in `git diff`. The line `book.go:62` is worth a look: the profile's hot call to `account` used to be there, the guard pushed it down, and the compiler now treats the `r.Adjust` call that moved onto that line as that hot call site.

The compiler's own decisions agree. Built with this profile, `v1` gets 11 hot-budget inlining decisions and 2 PGO devirtualizations, and `v2` gets 3 and none (`examples/logs/`).

The `comments` version only adds comments, reorders functions in `book.go` and moves `normalize` to its own file. Line offsets inside functions don't change, so it stays quiet:

    $ go run ./plugin/scripts check -C examples/ledger/comments --profile examples/profiles/ledger-v1.pprof
    Profile examples/profiles/ledger-v1.pprof on example.com/ledger
    Profile: 119 call edges, 104 hot, total weight 1565.
    In this module: 30.6% of the weight (17 hot edges). Standard library and other modules: 68.7%, not checked. Calls to compiler-inserted runtime helpers: 0.7%, skipped.
    
    Still matches: 100.0% of this module's hot weight (17 of 17 hot edges), 100.0% of all its weight.
    Every call edge in this module still matches the code.

A profile from `go test -bench Run -cpuprofile` in package main never matched the binary. The test binary names package main by its import path, so the parts of the profile in package main don't apply to the real build. Only the other packages still match:

    $ go run ./plugin/scripts check -C examples/ledger/v1 --profile examples/profiles/ledger-v1-test.pprof | tail -n +5
    Still matches: 31.8% of this module's hot weight (9 of 16 hot edges), 31.8% of all its weight.
    Below the minimum of 90%: the profile is stale for this code. Collect a new one from the current build.
    
    Functions in the profile that this build doesn't have:
      example.com/ledger.BenchmarkRun  weight 73 (6.6%), 2 hot edges
          a test function: the profile came from a go test binary, which this build doesn't include
      example.com/ledger.Digest  weight 60 (5.4%), 1 hot edge
          the profile names package main by its import path, as a go test binary does, but the program's build calls it main.Digest; collect the profile from the real binary
      example.com/ledger.Run  weight 18 (1.6%), 2 hot edges
          the profile names package main by its import path, as a go test binary does, but the program's build calls it main.Run; collect the profile from the real binary
      example.com/ledger.Run.func1  weight 8 (0.7%), 2 hot edges
          the profile names package main by its import path, as a go test binary does, but the program's build calls it main.Run.func1; collect the profile from the real binary

`edges` shows a profile's heaviest call edges the way the compiler reduces them. A `*` marks hot edges:

    $ go run ./plugin/scripts edges --top 6 examples/profiles/ledger-v1.pprof
    119 call edges, 104 hot, total weight 1565
    *   8.05%  main.main +20 -> main.Digest
    *   7.99%  main.Digest +3 -> hash/fnv.(*sum64a).Write
    *   5.11%  runtime.wbBufFlush1 +45 -> runtime.tryDeferToSpanScan
    *   4.22%  example.com/ledger/parse.normalize +1 -> strings.ToLower
    *   4.15%  runtime.growslice +106 -> runtime.memmove
    *   3.83%  runtime.bulkBarrierPreWriteSrcOnly +24 -> runtime.typePointers.next
    ... 113 more

### Real data: the Go compiler's own profile

The Go compiler commits a `default.pgo` in `src/cmd/compile`. Run on Go 1.27.3's compiler source, its own profile still matches 99.1% of the hot weight in the `cmd` packages (`examples/real/cmd-compile-go1.27.3-own-profile.txt`). The `default.pgo` from Go 1.22.12, checked against the same Go 1.27.3 source, matches 67.3%:

    Profile /home/builder/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.22.12.linux-arm64/src/cmd/compile/default.pgo on cmd/compile
    Profile: 3189 call edges, 3051 hot, total weight 13877.
    In this module: 48.9% of the weight (2021 hot edges). Standard library and other modules: 43.6%, not checked. Calls to compiler-inserted runtime helpers: 7.5%, skipped.

    Still matches: 67.3% of this module's hot weight (1323 of 2021 hot edges), 67.3% of all its weight.
    Below the minimum of 90%: the profile is stale for this code. Collect a new one from the current build.

    Functions in the profile that this build doesn't have:
      cmd/compile/internal/ssa.decomposeBuiltIn  weight 47 (0.3%), 3 hot edges
          probably renamed to cmd/compile/internal/ssa.decomposeBuiltin, which makes the same calls at the same lines
      cmd/compile/internal/ssa.sortvalues.Less  weight 34 (0.2%), 1 hot edge
          renamed, removed, or has no calls left
      cmd/compile/internal/ssagen.byStackVar.Less  weight 12 (0.1%), 1 hot edge
          renamed, removed, or has no calls left
      cmd/compile/internal/liveness.(*liveness).epilogue  weight 12 (0.1%), 6 hot edges
          probably now cmd/compile/internal/liveness.(*Liveness).epilogue, with the same name and a different receiver
      cmd/compile/internal/types2.(*Scope).LookupParent  weight 11 (0.1%), 2 hot edges
          renamed, removed, or has no calls left
      cmd/compile/internal/dwarfgen.createComplexVars  weight 11 (0.1%), 3 hot edges
          renamed, removed, or has no calls left
      cmd/compile/internal/ssa.(*poset).newconst  weight 10 (0.1%), 3 hot edges
          renamed, removed, or has no calls left
      cmd/compile/internal/ssa.layoutRegallocOrder  weight 8 (0.1%), 1 hot edge
          renamed, removed, or has no calls left
      cmd/compile/internal/liveness.(*liveness).compact  weight 8 (0.1%), 5 hot edges
          probably now cmd/compile/internal/liveness.(*Liveness).compact, with the same name and a different receiver
      cmd/compile/internal/ssa.(*Cache).allocInt64Slice  weight 6 (0.0%), 1 hot edge
          renamed, removed, or has no calls left
      ... 29 more (use --limit 0 or --json)

Some findings, checked against both source trees: `decomposeBuiltIn` was renamed to `decomposeBuiltin`, `liveness.(*liveness)` became `liveness.(*Liveness)`, and the `s.computeLive()` call in `(*regAllocState).init` moved from offset 111 to 117.

### Real data: gojq

[gojq](https://github.com/itchyny/gojq) is a jq implementation in Go. A 10-second CPU profile of v0.12.17 matches 100% on v0.12.17 and 62.7% on v0.12.20, released two years later (`examples/real/gojq/`). The findings were checked against both versions: `normalizeNumber` became `parseNumber` with the same body, `binopTypeSwitch` became generic, and `(*env).Next` now starts one line lower, so the profile's offset for `env.pushfork(pc)` lands on the `env.push(xs[1:])` call above it.

## Usage

    pgostale check [--profile FILE] [--min PCT] [--fail] [--json | --markdown] [--limit N] [--save FILE] [-C DIR] [PACKAGE]
    pgostale edges [--top N] [--json] PROFILE

- `check` builds `PACKAGE` (default `.`, normally the main package) with `go build -pgo=PROFILE -o /dev/null` and the compiler's PGO debug output turned on for your module's packages. The profile defaults to `default.pgo` in the package directory, which is what `go build` uses.
- `--min` is the share of hot weight that must still match, 90 by default. `--fail` exits 1 when the match is below it.
- `--json` prints the full result, including every lost edge. `--markdown` prints the match share and separate tables for missing functions, moved call sites and changed calls, suitable for a pull request comment. These two output formats cannot be combined.
- `--limit` caps each text or Markdown section, 15 by default, 0 for all. Markdown escapes names and paths so they stay inside their table cells. The report describes compiler profile matching, not a measured slowdown.
- `--save FILE` keeps the build output with the compiler's per-call-site JSON lines.
- `-C DIR` runs in another directory, like `go -C`. A relative `--profile` path is still read from the current directory.
- Exit code 2 means it couldn't check, and nothing is printed on stdout: an empty, cut-off or non-CPU profile, too few samples, a package that doesn't build, or a profile with nothing in the module that still matches.

### In CI

    - run: go run github.com/GillMacedo/pgostale/plugin/scripts@v0.1.0 check --fail ./cmd/server

This hasn't been run in GitHub Actions yet.

## What it reports

| Section | Meaning | Counts against the match |
|---|---|---|
| Functions this build doesn't have | The profile's caller name isn't in the build, with a hint: moved to another package, receiver changed, closure renumbered, probably renamed, or a `go test` profile | yes |
| Call sites that moved | The function exists, but no call sits at the profile's line offset any more | yes |
| Calls X now | The compiler still finds a call at that offset, but it calls a different function | yes |
| Standard library and other modules | Edges whose caller isn't in your module | not checked |
| Compiler-inserted runtime helpers | Edges to things like `runtime.growslice` or `gcWriteBarrier`, which have no call in the source | skipped |

Hot edges are the heaviest edges that together make up 99% of the profile weight, the same cut the compiler uses for PGO inlining. The match share counts hot edges whose caller is in your module.

## Limits

- **Matching isn't speed.** A matched call site is one the compiler can use. Whether that makes code faster depends on the workload. Measure with and without `-pgo=off`.
- **Only your module.** Edges whose caller is in the standard library or a dependency aren't checked, so drift after a Go or dependency upgrade isn't shown yet.
- **A representative profile is your job.** A profile can match the code perfectly and still come from a test load that looks nothing like production.
- **Debug output.** The compiler's `-d=pgodebug=3` output is a debug feature. If a Go release stops printing it, `pgostale` refuses to report instead of calling the profile fine.
- **Hints are guesses.** "Probably renamed to" needs at least two calls at the same offsets. A missing function with no matching hint may have been renamed and changed at the same time.
- **Builds the code.** `go build` follows your Go settings, so it may download modules. `pgostale` itself never uses the network and never runs the program.
- **A crash can leave a valid profile.** A program that panics with `defer pprof.StopCPUProfile()` still writes a complete profile of the time before the crash. `pgostale` refuses it only if it has fewer than 20 samples in call edges. A program that exits through `log.Fatal` or `os.Exit` leaves an empty file, which is refused.
- **No preprofile text.** Files made by `go tool preprofile` can't be checked for truncation, so they are refused. Pass the pprof file instead.
- Paths in the report point at the files the compiler read. With `-trimpath` in `GOFLAGS`, they may not exist on disk, and call names and `func` lines can't be read.

## FAQ

**How do I know if my default.pgo is stale?** Run `pgostale check` on the main package. It calls the profile stale below 90% of the hot weight matching. That cut is a default, so set `--min` to what your team accepts.

**Does Go warn when a PGO profile doesn't match the code?** No. In every build tested here, `go build` used what still matched and ignored the rest without a message.

**How often should I refresh a PGO profile?** The Go PGO guide describes collecting a profile from production, rebuilding and repeating as a cycle. `pgostale check --fail` in CI tells you when the code has drifted far enough that it matters.

**Can I use a profile from `go test -cpuprofile`?** For hot code outside package main, yes, if the benchmark is representative. For code in package main, no: the test binary names it by import path, so it never matches the real binary.

**Why does adding a few lines break PGO?** PGO keys a call site by its line offset from the `func` line. Lines added above a call shift every offset below them.

**How is this different from pgoctl?** `pgoctl validate` scores a profile's sample count and coverage, and `leverage-check` compares builds with and without PGO. `pgostale` checks the profile against the current source and names each function and call site that no longer matches.

**Is it an MCP server?** No. It's a skill with one Go source file, so `go run` is all it needs.

## Tested on

- **Real profiles** of `examples/ledger/v1`, from the binary and from `go test -bench`, checked against `v1`, `v2` and `comments` on Go 1.27.3 (linux/arm64) and Go 1.22.12. Both versions give the same results.
- **The Go compiler's own `default.pgo`**, from Go 1.27.3 and from Go 1.22.12, on Go 1.27.3's `cmd/compile` source. Findings were spot-checked against both source trees.
- **gojq**: a profile of v0.12.17 checked on v0.12.17 and v0.12.20, with findings checked by hand.
- **False positives found on real data and fixed:** assembly helpers such as `cmpbody` reached through string compares, deferred calls that the profile places on the return line, a closure whose start line was ambiguous from weights alone (now limited to lines with a `func` or `range` keyword), and a "same name" hint that pointed at an unrelated method.
- **The pprof reader** matches `go tool preprofile` edge for edge on the ledger, gojq and Go compiler profiles. Every cut point of a real profile, gzipped and raw, is refused or gives the same edges as the whole file.
- **Broken input** in `tests/`: empty, garbage, cut-off, heap, goroutine, idle and preprofile-format profiles, profiles left by programs that failed while profiling (through `log.Fatal`, and through a panic after a fraction of a second), `go build` error output given as a profile, and source trees with a syntax error, a file cut partway, an empty file, a type error, no Go files, no `go.mod`, or a different program. All of them exit 2 with nothing on stdout, with and without `--fail`.

It has only run on a linux/arm64 machine. Builds of the ledger examples that target amd64, 386 and riscv64 through `GOARCH` gave the same results, and a test repeats this with another architecture. It hasn't been tested in a live Claude Code session, on Windows or macOS, in GitHub Actions, with cgo packages, with `go.work` workspaces, with `-trimpath`, or through `go run ...@latest`, which needs the published repo.

## Contributing

Contributions are welcome, from people and from people working with AI agents. See [CONTRIBUTING.md](CONTRIBUTING.md). Good first issues are [listed here](https://github.com/GillMacedo/pgostale/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22).

### How pull requests work

1. **Open a PR.** Fork the repo, work on a branch, and keep it to one change. Write a title that says what changes for users, because it becomes the changelog line: `feat:` goes under Added and `fix:` under Fixed.
2. **Automatic check.** Code changes need tests, and `make test` must pass. Changes over 30 lines need a linked open issue (`Closes #<number>`), and a PR can have at most 800 changed lines. If something is missing, a comment tells you what to fix.
3. **Review.** Claude reviews each PR automatically. Code PRs also get a security review and are tested in a sandbox. The review then merges the PR or asks for changes, and a small problem may become a follow-up issue instead. A PR with no new commit 14 days after a change request is closed, and you can reopen it. PRs that change CI, agent settings, npm or Python dependencies, vendored code or build files can't be merged this way, and a comment says so. The same goes for anything the security review doesn't clear.
4. **Release.** A merge that changes what users install becomes a new version within about two hours, with a GitHub Release and, if the project has one, its npm package. Docs and CI changes wait for the next release.

## AI disclosure

This project was largely generated with Claude Code, working on its own, and is published by its maintainer.

## About me

I'm Giulliano Macedo, and I build developer tools at GoTech Solutions. Follow along or get in touch:

- GitHub: [GillMacedo](https://github.com/GillMacedo)
- LinkedIn: [giulliano](https://www.linkedin.com/in/giulliano/)
- X: [@giumacedo](https://x.com/giumacedo)

If `pgostale` saved you some prompts, a star helps others find it.

---

Built by [Giulliano Macedo](https://github.com/GillMacedo), GoTech Solutions · [LinkedIn](https://www.linkedin.com/in/giulliano/) · [X](https://x.com/giumacedo)
