# Local model-as-a-service simulator (development only)

AI-SDLC depends on exactly one external capability: a model-as-a-service
reached through a standard API. Every other part of the SDLC flow is
provided by AI-SDLC's own components: admission, planning, isolated
generation, behavior verification, independent review, repair, publication,
build, provenance and deployment.

When no hosted model service is available, `agent_cli_openai_service.py`
simulates one on the developer host. It serves the standard OpenAI Chat
Completions API and translates each call to a local agent CLI, Claude Code,
used purely as a completion model. The installation in kind does not know about
the CLI. It uses the regular `openai` gateway adapter against this endpoint,
like any other model service. No agent CLI, provider credential or wrapper
code enters the cluster or its images.

```
ai-sdlc (kind)  --OpenAI API (allowlist/token)-->  host service  -->  claude -p (no tools)  -->  provider
  worker/verifier gateways (openai adapter)   10.89.0.1:18090     per call, isolated        host credentials
```

## Contract

- `POST /v1/chat/completions`: an OpenAI request becomes a `chat.completion`
  with native `tool_calls` and `usage`. A request with `stream: true` (and
  optionally `stream_options: {"include_usage": true}`) gets the same answer
  framed as server-sent events: the CLI answers at once, so only the framing
  changes. The gateway's `openai` adapter streams to guard local models.
- `GET /v1/models`: the configured aliases.
- Authentication: a local bearer token (`tokenFile`, not a provider key),
  and/or an allowlist of client addresses (`allowedClients`, for example the
  kind node on the bridge). The installation's credential inventory is
  closed, so until the installer manages a service Secret, use the client
  allowlist. Inside the cluster, the chart's egress policy (enforced only
  when the cluster's network plugin applies NetworkPolicy) selects pods, not
  containers: every container sharing a gateway pod's network, such as
  `verifier` beside `model-gateway`, can reach the endpoint, and an address
  allowlist cannot tell them apart. In kind, pod traffic to the bridge is
  usually masqueraded to the node address, so allowlisting the node admits
  every pod and process on that node: allowlist-only access is no stronger
  than NetworkPolicy enforcement. Where access must be limited to the
  gateway itself, use an independent credential (`tokenFile`) held only by
  the gateway.

Each call runs `claude -p` in a fresh empty directory with a minimal
environment:
- no native tools (`--tools ""`);
- no session, settings, hooks, MCP servers, slash commands or CLAUDE.md;
- the model has no tool at all, so it cannot attempt native calls. Its answer
  must start with one JSON turn (`content`, `requested_actions`), validated
  strictly and by the same completion contract as the gateways (tool
  allowlist, object arguments and size bounds). Text after that object, such
  as a continuation the model imagined, is ignored. A plain answer with no
  structure is a content-only (final) turn. Any other structured-looking
  answer, or one that ends by announcing an action it never requested, is
  retried once within the call's time budget and then answered with 502.

The agent's system messages go in the system prompt. Transcript turn headers
carry a random per-call marker, so text inside files or tool results cannot
impersonate a turn. Backend failures return 502, invalid requests 400 and
saturation 429.

## Run

```json
{"address": "10.89.0.1", "port": 18090, "allowedClients": ["10.89.0.5"],
 "binary": "/root/.local/share/claude/versions/2.1.329", "seconds": 300, "concurrency": 2,
 "models": {"claude-sonnet": "sonnet"},
 "environmentValues": {"CLAUDE_CODE_USE_VERTEX": "1", "ANTHROPIC_VERTEX_PROJECT_ID": "<project>",
                       "CLOUD_ML_REGION": "global",
                       "GOOGLE_APPLICATION_CREDENTIALS": "/root/.config/gcloud/application_default_credentials.json"}}
```

```bash
python3 tools/local_model_service/agent_cli_openai_service.py --config /etc/ai-sdlc-local-maas/config.json
```

Bind only to the kind bridge address. In the installation, select the `openai`
adapter with `url: http://10.89.0.1:18090/v1/chat/completions`, `cidr:
10.89.0.1/32`, `port: 18090` and the model alias.

## Evidence

An offline trial on Saferus issue #4 used the real executor profile (600 s /
24 steps) and reviewer profile (180 s / 8 steps):
- generation succeeded in 207 s, with only the three requested files changed
  and tests passing;
- the independent review approved in 122 s.

A JSON-serialized conversation exhausted its steps instead, and rewrote an
unrelated file from memory.
