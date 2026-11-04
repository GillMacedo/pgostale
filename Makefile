.PHONY: test lint demo profiles

RUN = go run ./plugin/scripts
PROFILE = examples/profiles/ledger-v1.pprof

test:
	test -z "$$(gofmt -l plugin tests examples)"
	go vet ./...
	for d in v1 v2 comments; do (cd examples/ledger/$$d && go vet ./...) || exit 1; done
	go test -count=1 ./...

lint:
	claude plugin validate ./plugin --strict
	claude plugin validate . --strict

# v2 is below the default minimum on purpose, so it runs without --fail.
demo:
	$(RUN) check -C examples/ledger/v1 --fail --profile $(PROFILE)
	$(RUN) check -C examples/ledger/v2 --profile $(PROFILE)

# Records new profiles of the v1 ledger: one from the binary and one from
# go test. Profiles differ on every run, so update README.md afterwards.
profiles:
	cd examples/ledger/v1 && go build -o /tmp/pgostale-ledger . && /tmp/pgostale-ledger -seconds 10 -cpuprofile ../../profiles/ledger-v1.pprof
	cd examples/ledger/v1 && go test -run '^$$' -bench Run -benchtime 5s -cpuprofile ../../profiles/ledger-v1-test.pprof -o /tmp/pgostale-ledger.test .
