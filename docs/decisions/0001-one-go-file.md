# 0001: One standard-library Go file in a skill

**Decision:** the tool is a single Go file with no dependencies, run with `go run`, bundled in a Claude Code skill.

**Why:** anyone with a `default.pgo` has Go, so `go run` is the whole install. An agent can already run a script and read files, so an MCP server would add a binary and an npm package without giving the agent anything new. The pprof reader is about 300 lines, which is less than vendoring `github.com/google/pprof/profile`.

**Alternatives:** an MCP server with the official Go SDK, like `stuckgo`. It would make sense if agents needed this without a shell, which isn't the case for a tool that builds code.
