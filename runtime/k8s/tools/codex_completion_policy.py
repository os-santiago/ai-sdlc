"""Reviewed completion-only policy for the pinned local Codex CLI adapter."""
import json
import hashlib
import os
import stat
from functools import lru_cache
from pathlib import Path

SUPPORTED_VERSION = "codex-cli 0.169.0"
MODEL = "gpt-6.1-sol"
POLICY_HASHES = {
    "codex_completion_catalog.json": "b943cf4344445dc7559548f70cd8853765501df0e4dfde35e49c5db2f7e621a8",
    "codex_completion_instructions.txt": "1f79a293c810c97dd5adaa8fc98cf332f08314fbc28a704b2559629520d736d9",
}
DISABLED_FEATURES = (
    "shell_tool", "unified_exec", "multi_agent", "multi_agent_v2", "apps", "plugins",
    "remote_plugin", "hooks", "memories", "skill_search", "skill_mcp_dependency_install",
    "code_mode", "code_mode_only", "code_mode_host", "code_mode_prewarm", "computer_use",
    "browser_use", "browser_use_external", "in_app_browser", "image_generation", "view_image",
    "sleep_tool", "tool_suggest", "daemon_auto_start", "request_permissions_tool",
    "token_budget", "context_management", "send_message_to_user_async", "auth_elicitation", "goals",
    "enable_request_compression",
)


@lru_cache(maxsize=1)
def policy_snapshot():
    root = Path(__file__).resolve().parent
    snapshot = []
    for name, expected in POLICY_HASHES.items():
        descriptor = os.open(root / name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(descriptor, "rb") as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or not 0 < info.st_size <= 64 * 1024:
                raise RuntimeError("unsafe completion policy")
            raw = stream.read(64 * 1024 + 1)
        if hashlib.sha256(raw).hexdigest() != expected:
            raise RuntimeError("completion policy digest mismatch")
        snapshot.append((name, raw))
    return tuple(snapshot)


def stage_policy(directory):
    for name, raw in policy_snapshot():
        descriptor = os.open(Path(directory) / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(raw)


def completion_args(binary, directory, schema, output, policy_directory=None):
    root = Path(policy_directory) if policy_directory else Path(__file__).resolve().parent
    args = [str(binary), "exec", "--ephemeral", "--ignore-user-config", "--strict-config",
            "--sandbox", "read-only", "--model", MODEL, "--skip-git-repo-check", "-C", str(directory),
            "--output-schema", str(schema), "--output-last-message", str(output)]
    for feature in DISABLED_FEATURES:
        args += ["--disable", feature]
    args += ["--enable", "skip_host_skill_discovery"]
    # Built-in OpenAI ignores configured retry overrides in CLI 0.169.0.
    # A dedicated provider keeps existing OpenAI authentication/default routing
    # while explicitly disabling request/stream retry and WebSocket fallback.
    args += ["-c", 'model_provider="ai_sdlc_completion"', "-c",
             'model_providers.ai_sdlc_completion={name="OpenAI",wire_api="responses",'
             'requires_openai_auth=true,supports_websockets=false,'
             'request_max_retries=0,stream_max_retries=0,stream_idle_timeout_ms=20000}']
    for key, value in {
        "approval_policy": "never", "web_search": "disabled", "project_doc_max_bytes": 0,
        "tools.update_plan.enabled": False, "tools.experimental_request_user_input.enabled": False,
        "skills.bundled.enabled": False, "skills.include_instructions": False,
        "plugins._default.enabled": False, "model_reasoning_effort": "low",
        "model_catalog_json": str(root / "codex_completion_catalog.json"),
        "model_instructions_file": str(root / "codex_completion_instructions.txt"),
    }.items():
        # JSON scalar literals are valid TOML values; these are argv elements,
        # never shell text. Paths and user data cannot add executable arguments.
        args += ["-c", key + "=" + json.dumps(value)]
    return args + ["-"]
