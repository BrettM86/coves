# Coves

Coves is an open-source forum platform built on the AT Protocol. It has
communities, threaded comments, voting, and subscription feeds.

This repository contains the Go backend (AppView), the `social.coves.*` lexicon
schemas, and content aggregators. Coves is under active development.

## How it works

Personal Data Servers (PDSes) store repository records and blobs. The AppView
indexes record changes from Jetstream into PostgreSQL and serves XRPC APIs to
clients. Writes go through PDS APIs.

New posts live in their authors' repositories. Communities publish acceptance
and removal records to control which posts appear in the community.

## Repositories

| Repository | Purpose |
| --- | --- |
| **coves** (this repository) | Go AppView, lexicons, and aggregators |
| [coves-frontend](https://tangled.org/bretton.dev/coves-frontend) | SvelteKit web client |
| [coves-mobile](https://tangled.org/bretton.dev/coves-mobile) | Flutter mobile client |
| [tidepool](https://tangled.org/bretton.dev/tidepool) | Federation bridge |

Backend source is available on [Tangled](https://tangled.org/bretton.dev/coves)
and [GitHub](https://github.com/BrettM86/coves).

## Start contributing

Install Git, Make, and the Go toolchain specified in [go.mod](go.mod), then run
the unit tests:

```sh
git clone https://tangled.org/bretton.dev/coves
cd coves
make test
```

Unit tests need no Docker services. Go downloads dependencies on the first run.

See [CONTRIBUTING.md](CONTRIBUTING.md) for setup and pull requests, or browse
the [documentation](docs/README.md).

## Bugs and security

Report bugs or propose changes through the
[issue tracker](https://tangled.org/bretton.dev/coves/issues).

Report suspected vulnerabilities privately using [SECURITY.md](SECURITY.md).

## License

Coves is licensed under the GNU Affero General Public License, version 3.
See [LICENSE](LICENSE) for the license text.
