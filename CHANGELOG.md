# Changelog

All notable changes are documented here, in [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. This project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-04

### Added
- `check` command: builds a package with its PGO profile, reads which call sites the compiler matched (`-gcflags=-d=pgodebug=3`), and reports the share of the module's hot profile weight that still applies
- Lists functions the build no longer has, with a hint: moved to another package, receiver changed, closure renumbered, probably renamed, or a profile from a `go test` binary
- Lists call sites that moved inside a function, with the line the profile now points at, and lines where the compiler still applies a hot call site to a different call
- `edges` command: lists a profile's heaviest call edges the way the compiler reduces them
- Refuses empty, cut-off, non-CPU and preprofile-format profiles, failed builds, profiles with too few samples, and profiles where nothing in the module matches (exit code 2)
- `--min`, `--fail`, `--json`, `--limit`, `--save` and `-C` options
- Claude Code plugin with the `checking-go-pgo-profiles` skill
