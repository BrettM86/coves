# Lexicon test data

Hand-written record samples, one JSON file per case, in one directory per record family. `tests/lexicon_fixtures_test.go` (T0, so part of `make test` and `make ci`) and `cmd/validate-lexicon` both walk this tree.

## Rules

- Every fixture has a top-level `$type` naming the record schema it is validated against. A `$type` that does not resolve fails the test.
- Name files `{type}-valid[-{variant}].json` or `{type}-invalid-{reason}.json`. A basename containing `-invalid-` must fail validation; anything else must pass. Both hyphens matter: `foo-invalid.json` is a must-pass case.
- Every `-invalid-` fixture needs an entry in `invalidFixtureExpectedErrors` in `tests/lexicon_fixtures_test.go`, keyed by basename, with a substring of the expected error. Without one the test fails. The entry stops an invalid fixture from passing because it broke for an unrelated reason.
- Each directory listed in `expectedFixtureFamilies` must keep at least one valid fixture, and each family in `familiesRequiringInvalidFixtures` at least one invalid one. A new directory counts only once it is added to the list.

## Running

```sh
go run ./cmd/validate-lexicon                 # schemas and fixtures
go run ./cmd/validate-lexicon -v              # list each file
go run ./cmd/validate-lexicon -strict         # strict datetime validation
go run ./cmd/validate-lexicon -schemas-only   # skip the fixtures
```
