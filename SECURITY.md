# Security policy

ezbx is an early `0.x` release, currently verified on Windows 11 with Docker Desktop.

## Reporting a problem

Please use GitHub's **private vulnerability reporting** on this repository (Security tab, then "Report a vulnerability"). Don't open a public issue for a security problem.

## Secrets and personal data

This repository must never contain API keys, tokens, `.env` files, personal paths or machine-specific configuration. A privacy check runs before every commit and in CI. If you find something that slipped through, report it the same way.
