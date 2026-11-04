# Security

Report vulnerabilities through [GitHub's private vulnerability reporting](https://github.com/GillMacedo/pgostale/security/advisories/new), not in a public issue.

`pgostale` runs `go list` and `go build` on the package you point it at. It never runs the built program and never uses the network itself, but `go build` follows your Go settings, so it may download modules and, for cgo packages, run the C compiler. Run it only on code you would build anyway.

A wrong match result is a normal bug. Report it privately only if it could lead someone into an unsafe action.
