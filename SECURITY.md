# Security policy

## Supported versions

Coves does not maintain multiple release lines. Security fixes land on `main`; operators should update to the newest revision.

## Reporting a vulnerability

Do not disclose a suspected vulnerability in a public issue, discussion or pull request before maintainers have had a chance to investigate.

Email `support@coves.social` with the subject `Security vulnerability`. This is the only private reporting channel; GitHub private vulnerability reporting is not enabled for this repository. Include, where possible:

- the affected component and revision;
- the impact and required preconditions;
- reproduction steps or a minimal proof of concept;
- any suggested mitigation; and
- how you would like to be credited.

Leave out real user data and production credentials, and avoid unnecessary access to third-party systems. Maintainers will coordinate validation, the fix and disclosure with the reporter.

How Coves handles authentication, firehose trust, outbound requests and stored credentials is described in [docs/SECURITY_MODEL.md](docs/SECURITY_MODEL.md), [docs/SSRF_SECURITY.md](docs/SSRF_SECURITY.md) and [docs/CREDENTIAL_ENCRYPTION.md](docs/CREDENTIAL_ENCRYPTION.md).
