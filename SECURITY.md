# Security Policy

## Supported Versions

goshs is effectively a rolling release. Fixes land as soon as possible and are
shipped in the next release, so **only the most recent release version is
supported**. Please reproduce any issue against the latest release before
reporting it.

## Reporting a Vulnerability

Please report security issues **privately** so they can be fixed before public
disclosure. Do **not** open a public issue for a suspected vulnerability.

Use GitHub's private vulnerability reporting:

- **https://github.com/goshs-labs/goshs/security/advisories/new**

(Repository → **Security** → **Advisories** → **Report a vulnerability**.)

To help triage quickly, please include as much of the following as you can:

- the goshs version (`goshs -v`) and the OS/architecture you run on,
- clear step-by-step instructions to reproduce,
- the exact request(s) made or a minimal Proof-of-Concept command line, and
- the impact you believe the issue has.

## Disclosure Process & Timelines

- **Acknowledgement:** I aim to acknowledge a report within **72 hours**.
- **Assessment:** an initial assessment of severity and validity typically
  follows within **7 days**.
- **Fix & release:** confirmed vulnerabilities are patched and released as soon
  as practical, prioritised by severity.
- **Coordinated disclosure:** once a fix is available, a
  [GitHub Security Advisory](https://github.com/goshs-labs/goshs/security/advisories)
  is published (with a CVE/GHSA identifier where applicable) crediting the
  reporter unless anonymity is requested. Please allow the fix to ship before
  disclosing details publicly.

Thank you for helping keep goshs and its users safe.
