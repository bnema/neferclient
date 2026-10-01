.PHONY: build test vet race mocks mocks-check fakes-check perf-check check

build:
	CGO_ENABLED=0 go build ./...
test:
	CGO_ENABLED=0 go test ./...
vet:
	CGO_ENABLED=0 go vet ./...
race:
	CGO_ENABLED=1 go test -race ./...
mocks:
	mockery
mocks-check: mocks
	git diff --exit-code -- mocks ':(glob)**/*_mock_test.go'
	@test -z "$$(git ls-files --others --exclude-standard -- mocks ':(glob)**/*_mock_test.go')"
fakes-check:
	@rc=0; grep -rniE '^\s*type\s+\w*(fake|stub|spy|dummy|mock)\w*|^\s*\w*(fake|stub|spy|dummy|mock)\w*(\[[^]]*\])?\s+(struct|interface)\b' --include='*_test.go' --exclude='*_mock_test.go' . 2>/dev/null || rc=$$?; \
	[ $$rc -eq 1 ] || { [ $$rc -eq 0 ] && printf '%s\n' 'handwritten test double: generate with Mockery v3' >&2; exit 1; }
perf-check:
	CGO_ENABLED=0 go test -run 'TestAlloc' -count=1 ./...
check: build vet test fakes-check perf-check
	CGO_ENABLED=0 staticcheck ./...
