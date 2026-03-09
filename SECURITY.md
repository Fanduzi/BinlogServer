# Security Policy

BinlogServer accepts responsible disclosure of security issues.

## Supported Versions

Security fixes are only guaranteed on the latest `main` branch state until a stable release line is established.

| Version | Supported |
| --- | --- |
| `main` | Yes |
| historical commits | No |

## Reporting a Vulnerability

Do not open a public issue for unpatched security problems.

Use one of these paths:

1. GitHub Security Advisory / private vulnerability report for this repository, if enabled.
2. A private maintainer contact channel already documented by the project team.

Include:

1. Affected version / commit
2. Impact summary
3. Reproduction steps or proof of concept
4. Suggested mitigation if known

The project will validate the report, work on a fix, and disclose publicly after a patch or mitigation is available.

## Hardening Guidance

Runtime security configuration guidance lives in [docs/security.md](docs/security.md), including:

1. API authentication
2. Secret management
3. Encryption support
4. Rate limiting
5. Production deployment checklist
