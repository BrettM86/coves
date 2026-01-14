# Coves Project Structure

This document provides an overview of the Coves project directory structure, following atProto architecture patterns.

**Legend:**
- 🔒 = Security-sensitive files

```
Coves/
├── CLAUDE.md                    # Project guidelines and architecture decisions
├── ATPROTO_GUIDE.md            # Comprehensive AT Protocol implementation guide
├── PROJECT_STRUCTURE.md        # This file - project structure overview
├── LICENSE                     # Project license
├── README.md                   # Project overview and setup instructions
├── go.mod                      # Go module definition
├── go.sum                      # Go module checksums
│
├── cmd/                        # Application entrypoints
├── internal/                   # Private application code
│   ├── xrpc/ †                 # XRPC handlers (atProto API layer)
│   ├── api/                    # Traditional HTTP endpoints (minimal)
│   ├── core/                   # Business logic and domain models
│   ├── atproto/                # atProto-specific implementations
│   └── config/ †               # Configuration management
│
├── db/                         # Database layer
│   ├── appview/ †              # AppView PostgreSQL queries
│   ├── postgres/               # Legacy/non-atProto database operations
│   ├── migrations/             # Database migrations
│   ├── local_dev_db_compose/   # Local development database
│   └── test_db_compose/        # Test database setup
│
├── pkg/                        # Public packages (can be imported by external projects)
├── data/                       # Runtime data storage
│   └── carstore/ 🔒            # CAR file storage directory
│
├── scripts/                    # Development and deployment scripts
├── tests/                      # Integration and e2e tests
├── docs/ †                     # Additional documentation
├── local_dev_data/             # Local development data
├── test_db_data/               # Test database seed data
└── build/ †                    # Build artifacts
```


## Development Guidelines

For detailed implementation guidelines, see [CLAUDE.md](./CLAUDE.md) and [ATPROTO_GUIDE.md](./ATPROTO_GUIDE.md).

1. **Start with Lexicons**: Define data schemas first
2. **Implement Core Domain**: Create models and interfaces
3. **Build Services**: Implement business logic
4. **Wire XRPC**: Connect handlers last