package main

import "testing"

// BenchmarkRun is the workload as a benchmark. A profile from
// `go test -bench Run -cpuprofile` names this package example.com/ledger,
// not main, which is why examples/profiles/ledger-v1-test.pprof doesn't
// apply to the ledger binary.
func BenchmarkRun(b *testing.B) {
	lines := Generate(20000)
	for i := 0; i < b.N; i++ {
		Run(lines)
		Digest(lines)
	}
}
