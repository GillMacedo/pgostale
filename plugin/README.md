# pgostale plugin

This plugin adds the `checking-go-pgo-profiles` skill to Claude Code. When you ask whether a Go PGO profile (`default.pgo`) is still current, Claude runs the bundled `scripts/pgostale.go` with `go run`. It builds the main package with the profile, reads which call sites the compiler matched, and reports the share of the module's hot profile weight that still applies, with the functions and call sites that were lost. Claude then reads the code for the biggest losses and says whether to collect a new profile. It needs Go 1.22 or newer. See the repository README for examples and limits.

Install it with `claude plugin marketplace add GillMacedo/pgostale` and then `claude plugin install pgostale@pgostale-marketplace`.
