Add a real-project example for: $ARGUMENTS

1. Pick an open-source Go program that commits a `default.pgo`, and check that its license allows including the profile and the report.
2. From the main package directory, run `go run <this repo>/plugin/scripts/pgostale.go check --limit 10 .` and save the output in `examples/real/`.
3. For each lost hot edge in the first lines, open the code at the profile's revision and the current one, and check the finding is real. Write down the commit, Go version and license in `examples/README.md`.
4. If a finding is wrong, add a failing test in `plugin/scripts/pgostale_test.go` first, then fix it and run `make test`.
