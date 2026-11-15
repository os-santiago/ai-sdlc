# Supervised Codex completion adapter

The local adapter translates SCC completion requests into structured responses
from `codex-cli 0.169.0`, using `gpt-6.1-sol`. SCC owns execution of proposed tools.
The adapter does not ingest issues, approve changes, advance workflow state,
create releases or deploy software. It is a supervised Linux development aid.

## Reviewed executable policy

Startup verifies the CLI version and the SHA-256 of the adjacent model catalog
and model instructions. The executable digest is checked again before each
completion; replacement at the same path fails until a reviewed restart.
Each invocation receives private copies of those policy bytes,
an output schema and a temporary working directory. The catalog disables shell,
patch and experimental model tools. CLI flags also disable applications,
plugins, hooks, skills discovery, multi-agent execution, browser access, goals,
planning, permission requests and other action features. The process uses
read-only sandbox mode and never receives an approval bypass.

This policy is based on the pinned
[Codex source](https://github.com/openai/codex/tree/rust-v0.169.0), including its
model catalog, feature registry and tool registration. Prompts alone do not
establish the execution boundary. The optional conformance test runs the actual
CLI against a local fake provider and asserts that both ordinary Responses
tools and Responses Lite `additional_tools` contain no executable tools.

`--ignore-user-config` excludes the user configuration file. System and managed
host configuration remain trusted dependencies: this flag does not isolate all
configuration sources or prove that host integrations cannot run during CLI
startup. Operators must review that configuration and run conformance on the
actual host. The executable, adapter files, Python interpreter, host identity
and existing Codex authentication are trusted. A version match alone is not an
executable signature or supply-chain attestation.
Use the standalone executable, not an npm or other launcher whose transitive
executables are outside this digest check. The host must prevent concurrent
mutation between verification and execution; this is not a hostile-host boundary.

## Setup and checks

Use the reviewed CLI version with existing local authentication. Installations
with another version fail before opening the listener. An absolute
`CODEX_WRAPPER_BINARY` may select the trusted executable. Set a random bearer
token of at least 32 characters in `CODEX_WRAPPER_TOKEN`, then run:

```sh
python3 runtime/k8s/tools/codexcli_openai_wrapper.py
```

The default listener is `127.0.0.1:8787`; `CODEX_WRAPPER_PORT` selects another
local port. Point SCC to `http://127.0.0.1:8787/v1`, model `codex-cli`, with the
same bearer. Do not expose this endpoint outside the supervised host.

```sh
python3 -m unittest discover -s runtime/k8s/tools -p 'test_*.py' -v
AI_SDLC_TEST_CODEX_BINARY=/absolute/path/to/codex \
  python3 -m unittest discover -s runtime/k8s/tools \
  -p 'test_codex_completion_policy.py' -v
```

The second command contacts only its local fake provider and intentionally
receives an error after capturing the tool inventory. Without the explicit
binary variable, that test reports a skip. Generic CI exercises the process,
filesystem and HTTP regressions without requiring model credentials. It does
not claim pinned-CLI conformance when the optional test is skipped.

## Bounds and failure behavior

| Boundary | Enforced limit |
| --- | --- |
| HTTP request line, headers and body | 3-second absolute read deadline |
| Authenticated request body | 128 KiB; one Content-Length; no transfer encoding |
| Parsed JSON | No duplicate case-insensitive keys, non-finite numbers or depth above 32 |
| Messages / declared tools | 256 / 64 |
| CLI invocation | 24-second wall deadline; request and stream retries disabled |
| Each child-written file | 8 MiB kernel file-size limit |
| Accepted completion file | 128 KiB, regular, one link, no symlink, strict UTF-8 |
| Completion content / proposals | 64 KiB / 8 calls, only declared tool names |
| Each proposed argument object | 32 KiB, strict JSON object |
| Serialized JSON or buffered SSE | 256 KiB |

The parent terminates its owned process group on success and failure. A minimal
environment excludes forge, registry, database, Kubernetes and wrapper bearer
credentials, while retaining existing model authentication and required host
paths. Errors returned to the HTTP client never include child diagnostics.
Buffered SSE begins only after the model finishes; it is not incremental model
streaming. One HTTPServer handles requests serially.

A dedicated provider selects existing OpenAI authentication and its default
routing, sets request and stream retries to zero and disables WebSocket use.
Overriding retry settings on the built-in `openai` provider is ineffective in
the pinned CLI, so the adapter does not use that configuration path. Actual-CLI
conformance also proves one request on HTTP 503 and a truncated response stream.
This does not prove exactly-once execution at the external service after an
uncertain network failure; the adapter reports failure without resubmission.

`max_tokens` is admission-checked (1–8192), not a measured or enforced model
billing budget. Temperature and tool-choice fields are compatibility inputs;
the fixed backend policy governs generation. Time and byte bounds do not impose
a total memory, total disk, network or aggregate spend limit. The process-group
cleanup does not contain a malicious process that detaches into another session.
Such isolation requires the container/cgroup model gateway and admission policy.

These checks establish a local component boundary. A successful external model
completion is not evidence of autonomous issue-to-release delivery, SCC tool
execution, production quality gates or a successful Homedir deployment.

The [real-model Homedir component benchmark](real-model-homedir-benchmark.md)
checks the adapter together with SCC, the durable broker and gVisor. Completion
policy explicitly separates the model host's read-only permissions from inert
JSON proposals for SCC's independently authorized workspace. No host action tool
or executor permission is added by this distinction.
