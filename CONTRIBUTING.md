# Contributing to Glovebox

Thank you for your interest in contributing to glovebox. This document explains
how to contribute effectively.

## Getting Started

1. **Fork** the repository on GitHub
2. **Clone** your fork: `git clone https://github.com/<you>/glovebox.git`
3. **Create a branch**: `git checkout -b feat/my-change`
4. **Make changes**, write tests, verify
5. **Commit** with sign-off: `git commit -s -m "feat: description"`
6. **Push** and open a pull request

## Development Setup

```sh
# Build
go build .
go build ./connectors/rss/

# Test
go vet ./...
go test ./... -count=1 -race

# Generate a new connector scaffold
go run ./generator new-connector my-source
```

## Code Standards

- **Language**: Go (1.26+)
- **No emoji in code** -- keep output ASCII-compatible
- **Tests**: write tests for all new code, prefer TDD (red-green-refactor)
- **Tooling**: `go vet` must pass, `go test -race` must pass
- **Dependencies**: prefer the standard library; new external deps need justification
- **Containers**: always rebuild to deliver code changes; never copy files into
  a running container
- **Secrets**: never commit credentials to git

## Contributing a Connector

The easiest way to contribute is to add a new connector:

1. Generate the scaffold: `go run ./generator new-connector <name>`
2. Implement the `Poll` method in `connector.go`
3. Use `connector.NewHTTPClient()` for HTTP requests (standardized User-Agent)
4. Use `FetchCounter.TryFetch()` to respect fetch limits
5. Set `Identity` on all staged items
6. Pass `RuleTags` from `MatchResult` to `ItemOptions`
7. Write tests using `httptest` mock servers
8. Include a Dockerfile following the distroless pattern
9. Update `config.json` with sensible defaults

Reference implementation: `connectors/rss/` (poll-only) or `connectors/github/`
(poll + webhook listener).

See `docs/connector-guide.md` for the full development guide.

## Pull Request Requirements

- **Clear description** of what the PR does and why
- **Tests** for all new code (aim for the exit criteria in the issue/bead)
- **Documentation** updated if behavior changes
- **`go vet` and `go test` pass** on all packages
- **One concern per PR** -- don't bundle unrelated changes

## Licensing and IP

All contributions to glovebox are licensed under the
[Apache License 2.0](LICENSE).

By submitting a pull request, you agree that:

- You have the right to submit the code under this license
- The code is your original work, or you have permission to contribute it
- You are not contributing code that belongs to someone else without their
  permission

We use the [Developer Certificate of Origin (DCO)](https://developercertificate.org/).
Sign off your commits with `git commit -s` to certify that you have the right
to submit them.

## What We Welcome

- Bug fixes with tests
- New connectors (see above)
- Documentation improvements
- Test coverage improvements
- Performance improvements with benchmarks

## What Needs Discussion First

Open an issue before submitting a PR for:

- Changes to the connector library API
- New library-level features
- Architecture changes
- Changes to the scanning engine or rules format

## AI-Assisted Contributions

This project uses agentic development tooling. AI-assisted contributions are
welcome and held to the same quality standards as any other contribution: tests
must pass, code must be clean, and the PR must be reviewable.

## Questions?

Open a [GitHub Discussion](https://github.com/leftathome/glovebox/discussions)
or file an issue.
