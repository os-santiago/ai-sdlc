# Install verified pull request publication

The Homedir pilot can explicitly enable publication after behavior verification
and independent review. Its reviewed installation JSON must use repository
`os-santiago/homedir`, profile `verification`, `"independentReview": true` and
`"publication": true`. Equivalent CLI options are `--profile verification
--independent-review --publication`. Publication is disabled by default.

Preview with `./install.sh plan --config <path>`. Apply with the same entry point,
`./install.sh local --config <path> --github-auth gh-cli`, using compatible,
reviewed published image pins. The first model installation also needs the
explicit `--codex-auth-file` option. No credential belongs in the JSON or Helm
values. An old image lock does not gain this feature merely by enabling a flag.

The installer imports a separate owned `ai-sdlc-publication-client` Secret.
Only release-manager mounts it. Intake retains its existing credential; the
verifier receives no GitHub credential. Release-manager receives public review,
execution and intake configuration, but no model token, model sidecar or
Kubernetes API token. Only its dedicated egress policy adds public HTTPS.

A successful independent review produces a signed component handoff. The relay
has exactly the five enabled internal routes, including `verify-artifact` and
`publish-candidate`. Unauthenticated requests cannot enqueue publication. The
release manager reads evidence under its own identity, reconstructs the exact
candidate and journals tree, commit, branch and PR effects. Ambiguous writes
quarantine publication; the installer cannot resume or adopt them.

The bounded pilot writes only the two reviewed locale-recipe files and enforces
`[skip release]` to keep Homedir's public VPS release workflow out of local
validation. Publication does not authorize merge, release or deployment.
Readiness reports `verified-publication-only`; it does not claim a completed
issue-to-production E2E.

Setting `publication` false removes the route, private credential mount and
publication egress policy, and transactionally revokes the publication table and
column privileges. Evidence and the owned credential Secret are retained.
Re-enabling cannot replay a terminal request. Apply changes after observing that
there is no active publication; disabling does not resolve uncertain effects.

## Validate installation changes

`tools/verify_publication_install.py` accepts only the dedicated disposable
`ai-sdlc-publication-install-test` kind node. Bootstrap that cluster with the
ordinary maintenance installer for Homedir. Build/import the eight changed
component images plus the model-mailbox fixture, and pass their immutable
references using `--images` along with explicit credential options and `--report`.
Locally imported fixture images are not published artifacts.

The tool keeps authority frozen and uses a distinct fixture intake label. It
checks enable, reapply, disable and re-enable through Helm with real PostgreSQL,
all seven deployments ready, exact publication grants, private mounts and routes,
and zero workflow requests. It never inserts or repairs workflow state. Retire
the owned disposable cluster after inspecting its sanitized report.

Separately, the publication controller conformance fixture now uses
`ProvisionRolesForCapabilities` to test its full signed handoff and evidence
queries with the exact installed column privileges. Its forge remains synthetic.
Actual autonomous GitHub publication and downstream promotion require their own
product-run evidence.

Published requests also retain [durable terminal evidence](published-records.md)
for later, separately authorized outerloop stages.
