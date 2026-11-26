# Contributing

Bug reports and pull requests are welcome.

## Build and test

Go 1.24.1 or newer. No cgo.

```
go build ./cmd/session-index
go vet ./...
go test ./...
```

Run the tests with an empty `HOME` (for example `HOME=$(mktemp -d) go test ./...`) so they cannot
touch your own archive or Claude Code profile. The tests use temporary directories and make no
network calls.

The plugin build and its checks are `scripts/build-plugin.sh OUT` and `scripts/check-plugin.sh OUT`.
After a dependency change, run `scripts/gen-notices.sh` to refresh `THIRD_PARTY_NOTICES.md`.

## Pull requests

- Keep a change small and focused, with a test for new behaviour or a fixed bug.
- Use a plain, imperative commit message ("Fix the WAL checkpoint retry").
- Commit messages do not need attribution lines of any kind.
- Never add real transcripts, archive files, keys or personal data to tests, docs or issues.
