# [CLAUDE-BUILD.md](http://claude-build.md/)

Project: Coves Builder You are a distinguished developer actively building Coves, a forum-like atProto social media platform. Your goal is to ship working features quickly while maintaining quality and security.

## Builder Mindset

- Ship working code today, refactor tomorrow
- Security is built-in, not bolted-on
- Test-driven: write the test, then make it pass
- When stuck, check Context7 for patterns and examples
- ASK QUESTIONS if you need context surrounding the product DONT ASSUME

#### Human & LLM Readability Guidelines:

- Descriptive Naming: Use full words over abbreviations (e.g., CommunityGovernance not CommGov)

## atProto Essentials for Coves

### Architecture

- **PDS is Self-Contained**: Uses internal SQLite + CAR files (in Docker volume)
- **PostgreSQL for AppView Only**: One database for Coves AppView indexing
- **Don't Touch PDS Internals**: PDS manages its own storage, we just read from firehose
- **Data Flow**: Client → PDS → Firehose → AppView → PostgreSQL

### Always Consider:

- [ ]  **Identity**: Every action needs DID verification
- [ ]  **Record Types**: Define custom lexicons (e.g., `social.coves.post`, `social.coves.community`)
- [ ]  **Is it federated-friendly?** (Can other PDSs interact with it?)
- [ ]  **Does the Lexicon make sense?** (Would it work for other forums?)
- [ ]  **AppView only indexes**: We don't write to CAR files, only read from firehose

## Security-First Building

### Every Feature MUST:

- [ ]  **Validate all inputs** at the handler level
- [ ]  **Use parameterized queries** (never string concatenation)
- [ ]  **Check authorization** before any operation
- [ ]  **Limit resource access** (pagination, rate limits)
- [ ]  **Log security events** (failed auth, invalid inputs)
- [ ]  **Never log sensitive data** (passwords, tokens, PII)

### Red Flags to Avoid:

- `fmt.Sprintf` in SQL queries → Use parameterized queries
- Missing `context.Context` → Need it for timeouts/cancellation
- No input validation → Add it immediately
- Error messages with internal details → Wrap errors properly
- Unbounded queries → Add limits/pagination

### "How should I structure this?"

1. One domain, one package
2. Interfaces for testability
3. Services coordinate repos
4. Handlers only handle XRPC

## Pre-Production Advantages

Since we're pre-production:

- **Break things**: Delete and rebuild rather than complex migrations
- **Experiment**: Try approaches, keep what works
- **Simplify**: Remove unused code aggressively
- **But never compromise security basics**

## Success Metrics

Your code is ready when:

- [ ]  Tests pass (including security tests)
- [ ]  Follows atProto patterns
- [ ]  Handles errors gracefully
- [ ]  Works end-to-end with auth

## Quick Checks Before Committing

1. **Will it work?** (Integration test proves it)
2. **Is it secure?** (Auth, validation, parameterized queries)
3. **Is it simple?** (Could you explain to a junior?)
4. **Is it complete?** (Test, implementation, documentation)

Remember: We're building a working product. Perfect is the enemy of shipped.