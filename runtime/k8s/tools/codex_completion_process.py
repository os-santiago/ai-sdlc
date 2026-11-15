"""Bounded Linux host subprocess support for the supervised completion adapter."""
import hashlib
import os
from pathlib import Path
import shutil
import signal
import stat
import subprocess
import sys
import tempfile

from codex_completion_policy import SUPPORTED_VERSION, completion_args, stage_policy

MAX_OUTPUT_BYTES = 128 * 1024
BACKEND_SECONDS = 24
_VERIFIED_BINARIES = {}


def child_environment():
    # Retain existing host authentication; never forward forge/registry/database
    # credentials, kubeconfig, wrapper bearer or ambient proxy/config overrides.
    allowed = ("PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TZ", "CODEX_HOME",
               "OPENAI_API_KEY", "CODEX_API_KEY", "SSL_CERT_FILE", "SSL_CERT_DIR")
    return {key: os.environ[key] for key in allowed if key in os.environ}


def run_process(args, prompt, timeout, stdout=subprocess.DEVNULL, cwd=None):
    launcher = Path(__file__).with_name("codex_completion_child.py")
    process = subprocess.Popen([sys.executable, "-I", "-S", str(launcher), *map(str, args)],
                               stdin=subprocess.PIPE, stdout=stdout, stderr=subprocess.DEVNULL,
                               env=child_environment(), start_new_session=True, bufsize=0, cwd=cwd)
    try:
        process.communicate(input=prompt, timeout=timeout)
        if process.returncode != 0:
            raise RuntimeError("completion backend failed")
    except subprocess.TimeoutExpired:
        raise RuntimeError("completion backend deadline exceeded") from None
    finally:
        # The group belongs to this child. Detached processes require a separate
        # container/cgroup boundary; this supervised adapter makes no such claim.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        if process.stdin is not None:
            process.stdin.close()
        try:
            process.wait(timeout=1)
        except subprocess.TimeoutExpired:
            raise RuntimeError("completion backend did not terminate") from None


def read_output(path, limit=MAX_OUTPUT_BYTES):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
    with os.fdopen(descriptor, "rb") as stream:
        before = os.fstat(stream.fileno())
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or not 0 < before.st_size <= limit:
            raise ValueError("unsafe completion output")
        data = stream.read(limit + 1)
        after = os.fstat(stream.fileno())
        if (len(data) != before.st_size or len(data) > limit or after.st_nlink != 1
                or (before.st_size, before.st_mtime_ns, before.st_ctime_ns)
                != (after.st_size, after.st_mtime_ns, after.st_ctime_ns)):
            raise ValueError("completion output changed")
        return data.decode("utf-8", errors="strict")


def binary_digest(binary):
    descriptor = os.open(binary, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
    with os.fdopen(descriptor, "rb") as stream:
        before = os.fstat(stream.fileno())
        if not stat.S_ISREG(before.st_mode) or not 0 < before.st_size <= 512 * 1024 * 1024:
            raise RuntimeError("unsafe completion executable")
        digest = hashlib.sha256()
        size = 0
        while block := stream.read(1024 * 1024):
            size += len(block)
            if size > 512 * 1024 * 1024:
                raise RuntimeError("completion executable changed")
            digest.update(block)
        after = os.fstat(stream.fileno())
        if size != before.st_size or (before.st_size, before.st_mtime_ns, before.st_ctime_ns) != (
                after.st_size, after.st_mtime_ns, after.st_ctime_ns):
            raise RuntimeError("completion executable changed")
        return digest.digest()


def verify_binary(binary):
    digest = binary_digest(binary)
    if binary in _VERIFIED_BINARIES:
        if digest != _VERIFIED_BINARIES[binary]:
            raise RuntimeError("completion executable changed; restart after review")
        return binary
    with tempfile.TemporaryDirectory(prefix="scc-codex-version-") as directory:
        output = Path(directory) / "version"
        with output.open("xb") as stream:
            run_process([binary, "--version"], b"", 2, stdout=stream, cwd=directory)
        if read_output(output, 128).strip() != SUPPORTED_VERSION:
            raise RuntimeError("unsupported completion backend version")
    if binary_digest(binary) != digest:
        raise RuntimeError("completion executable changed")
    _VERIFIED_BINARIES[binary] = digest
    return binary


def backend_binary():
    configured = os.environ.get("CODEX_WRAPPER_BINARY", "codex")
    found = shutil.which(configured)
    if not found:
        raise RuntimeError("completion backend unavailable")
    return verify_binary(str(Path(found).resolve(strict=True)))


def complete_process(prompt, schema):
    binary = backend_binary()
    with tempfile.TemporaryDirectory(prefix="scc-codex-completion-") as directory:
        stage_policy(directory)
        schema_path = Path(directory) / "response.schema.json"
        output_path = Path(directory) / "response.json"
        schema_path.write_text(schema, encoding="utf-8")
        run_process(completion_args(binary, directory, schema_path, output_path, directory),
                    prompt, BACKEND_SECONDS, cwd=directory)
        return read_output(output_path)
