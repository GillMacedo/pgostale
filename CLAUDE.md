# pgostale

Claude Code skill and Go CLI that checks whether a Go PGO profile (`default.pgo`) still matches the code it is applied to.

## Architecture
`plugin/scripts/pgostale.go` holds all the logic in one standard-library file, so `go run` needs no setup. `ReadProfile` decodes a pprof CPU profile and reduces it to call edges the way the compiler does. `Check` runs `go list` and then `go build -pgo=PROFILE -gcflags=MODULE/...=-d=pgodebug=3 -o /dev/null`. `ParseCallSites` reads the compiler's JSON call site lines, and `Match` compares them with the profile edges, using `fileSource` to name calls and find function start lines. `plugin/skills/checking-go-pgo-profiles/SKILL.md` tells Claude how to use it. See `docs/architecture.md`.

## Commands
- `make test`: gofmt, vet, unit tests and integration tests in `tests/`
- `make lint`: plugin and marketplace validation
- `make demo`: check the example profile against the healthy and the changed ledger
- `make profiles`: record new example profiles (they differ on every run, so update the README after)

## Conventions
- Standard library only. Never use the network, and never run the code being checked. `go list` and `go build` are the only things the tool does with a project.
- Anything the tool can't check exits 2 with nothing on stdout: an empty, cut-off or wrong profile, a failed build, or no matching call edges. Add a test for each new input.
- The compiler decides what matches. pgostale only reads its output and names what was lost.
- Profiles in `examples/profiles/` are real CPU profiles of `examples/ledger/v1`. Don't edit them by hand.

## Do and don't
- Do say that a lost hot call site means the compiler can't use that part of the profile. It isn't a measured slowdown.
- Don't call a profile fine because the report is 100%. Only call edges whose caller is in the module are checked.

## Definition of done
`make test` and `make lint` pass, and `ROADMAP.md`, the README and docs are updated when relevant.
