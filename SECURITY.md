# Security policy

Report potential vulnerabilities privately through GitHub's private
vulnerability reporting for this repository: the **Security** tab, then
**Report a vulnerability**. Do not include credentials, private keys, internal
endpoints, or exploit details in public issues.

For the trust boundaries, credentials and data flows of the runtime, see the
[security overview](docs/security-overview.md).

The active runtime is under `runtime/k8s/`. Security reports should identify
the affected component, deployment surface, observable impact, and a safe
reproduction path. Repository quality and security workflows under
`.github/workflows/` remain supported. Findings in the retired Actions
AI-SDLC runtime should identify the historical deprecation tag explicitly.
