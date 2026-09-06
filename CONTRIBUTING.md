# Contributing to Coves

Bug fixes, tests, and documentation improvements are welcome. For a new feature
or a change to federation, moderation, or privacy behavior, open an
[issue](https://tangled.org/bretton.dev/coves/issues) to discuss it first.

Bug reports should include reproduction steps, the affected revision, and what
happened versus what you expected. Remove credentials and personal data from
logs. Report vulnerabilities privately through [SECURITY.md](SECURITY.md).

## Development

Install Make and the Go version specified in [go.mod](go.mod). Run `make test`
to check your setup; it needs no external services.

Use an isolated worktree for feature work. From an up-to-date `main`:

```sh
git worktree add ../coves-my-change -b my-change main
cd ../coves-my-change
```

For local services, you'll also need Docker, the `docker-compose` command, and
`goose` for migrations. See the [local setup notes](docs/README.md#local-development)
before following the development guide.

## Tests

Read [TEST_ARCHITECTURE.md](docs/TEST_ARCHITECTURE.md) before writing tests.
Start behavioral changes with a failing test, and reproduce bugs before fixing
them. Put most coverage in unit and integration tests; use pipeline tests for
behavior that needs the full running system.

| Command | Runs |
| --- | --- |
| `make test` | Unit tests |
| `make lint` | Formatting checks and the pinned Go linter |
| `make test-integration` | Integration tests using local services |
| `make test-e2e` | Pipeline tests in an isolated Docker stack |
| `make ci` | The required merge gate, including all three test tiers |

The integration target starts the test database and uses `.env.dev`. Some
packages also need the local PDS started by `make dev-up`. Missing services and
unexpected skips are failures. See the [test command reference](docs/TEST_ARCHITECTURE.md#35-command-surface)
for prerequisites and the separate, opt-in live tests.

## Code changes

- Keep changes focused. Leave unrelated cleanup for another pull request.
- Put domain logic in `internal/core/`, XRPC handling in `internal/api/`, and
  database access in `internal/db/`.
- Use PDS APIs for record writes. Verify the acting DID and ownership before
  accepting a write; validate user input and remote records at the boundary.
- Use full words in names and return useful errors. Never log credentials or
  personal data.
- Update schemas, tests, and documentation when changing an API.
- Add new environment variables to [the test clear list](internal/config/testing.go).

For lexicon changes, run:

```sh
go run ./cmd/validate-lexicon
```

The [fixture guide](tests/lexicon-test-data/README.md) and
[fixture harness](tests/lexicon_fixtures_test.go) explain how to add examples.
Invalid fixtures also need an expected-error entry in the harness.

## AI-assisted contributions

AI and LLM tools are welcome. You are responsible for the changes you
submit: review properly, and run the relevant checks.

Keep pull requests focused and manageable to review. Around 1,000 changed
lines (additions plus deletions, including tests) is a guideline, not a hard
limit. Exclude generated files and lockfiles from that count. For larger
changes, consider splitting them into independently reviewable pull requests;
if the change needs to stay together, explain why in the description.

## Pull requests

Open a pull request against `main`. Explain the problem and what changed, link
any related issue, and list the checks you ran. Say which checks failed or
couldn't be run. Check documentation links against the files in the repository.

A passing `make ci` is required before merge. If you cannot run it locally,
mention that in the pull request. Maintainers review and squash changes into
`main`.
