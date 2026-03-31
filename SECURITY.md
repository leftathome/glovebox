# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in glovebox, please report it
responsibly. **Do not open a public GitHub issue.**

### Preferred: GitHub Security Advisories

Report via [GitHub Security Advisories](https://github.com/leftathome/glovebox/security/advisories/new).
This keeps the report private and allows us to coordinate a fix before disclosure.

### Alternative: Email

Send details to **security@leftathome.dev**.

### What to Include

- Description of the vulnerability
- Steps to reproduce
- Affected versions
- Potential impact
- Suggested fix (if you have one)

## Response Timeline

- **Acknowledge**: within 48 hours of report
- **Triage**: within 7 days (confirm validity and severity)
- **Patch**: within 30 days for critical/high severity, 90 days for medium/low
- **Disclosure**: coordinated with reporter after patch is released

## Scope

The following are in scope for security reports:

- **Glovebox scanner** -- scanning engine, routing, quarantine, audit
- **Connector library** -- staging writer, checkpoint, token management, webhook verification
- **First-party connectors** -- all connectors in the `connectors/` directory
- **Helm chart** -- Kubernetes deployment manifests, RBAC, network policies
- **CI/CD pipeline** -- GitHub Actions workflows, container image builds

The following are out of scope:

- Third-party connectors (report to their maintainers)
- Upstream dependencies (report to the dependency maintainer; we will update
  promptly once a fix is available)
- Social engineering or phishing attacks against maintainers

## Credit

Security reporters will be credited in release notes unless they prefer to
remain anonymous. Let us know your preference when reporting.

## Supported Versions

| Version | Supported |
|---------|-----------|
| 0.2.x   | Yes       |
| 0.1.x   | Security fixes only |
| < 0.1   | No        |
