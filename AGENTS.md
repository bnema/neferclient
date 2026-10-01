# Repository rules

- Go 1.27 only. Normal builds/tests use CGO_ENABLED=0; race tests use CGO_ENABLED=1.
- Library: return errors, never log. No application policy (shortcuts, which outputs to cover, when to lock).
- Owner goroutine: only the reader and the epoll waker run concurrently; all state changes happen in `Dispatch` on the caller's goroutine.
- Zero allocations in steady state (dispatch, present, release, key). Guard every hot path with a `TestAlloc*` test using `testing.AllocsPerRun`; `make perf-check` runs them.
- Mockery v3 testify generated mocks only; no handwritten fakes/stubs/spies. Public interfaces get mocks in `mocks/`; unexported seams get same-package `*_mock_test.go`. Add `.mockery.yml` entries and run `make mocks-check`.
- Secrets never become `string`, never reach logs, errors or `KeyEvent.Text`; scratch buffers are wiped after use.
- Signed conventional commits only. No push, tag or release without authorization.
- Local development resolves sibling modules through a gitignored `go.work`; committed `go.mod` never contains `replace`.
