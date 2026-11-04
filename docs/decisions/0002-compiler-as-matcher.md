# 0002: Let the compiler decide what matches

**Decision:** pgostale builds the package with `-gcflags=-d=pgodebug=3` and reads the compiler's own per-call-site weights, instead of re-implementing PGO matching from the source.

**Why:** PGO looks up a call site by the linker symbol name of the enclosing function and the line offset from its `func` line. Symbol names for methods, closures (`F.func1`, `F.func1.2`), defer wrappers, range-over-func bodies and generic instantiations follow compiler rules that change between releases. The compiler's answer is correct for the Go version the user builds with. The JSON format is the same in Go 1.22.12 and 1.27.3.

**Cost:** the flag is a debug flag. If a Go release stops printing these lines, pgostale gets no call site data and refuses to report (exit 2) rather than calling the profile fine. The source is still read, but only to name calls and find `func` lines for the report.

**Alternatives:** matching from `go/ast` alone. It would not need a build, but closure numbering and generic names would have to be guessed.
