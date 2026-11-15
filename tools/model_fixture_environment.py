"""Keep source/cluster fixture credentials outside completion adapters."""
import os


def provider_environment(port, token):
    # Match the ordinary completion process environment, adding only reviewed
    # local adapter configuration. Database, Kubernetes, GitHub and fixture
    # control credentials never reach this subprocess.
    allowed = ('PATH', 'HOME', 'USER', 'LOGNAME', 'LANG', 'LC_ALL', 'TZ',
               'CODEX_HOME', 'TMPDIR', 'TMP', 'TEMP', 'SSL_CERT_FILE',
               'SSL_CERT_DIR', 'CODEX_WRAPPER_BINARY',
               'OPENAI_API_KEY', 'CODEX_API_KEY')
    env = {key: os.environ[key] for key in allowed if key in os.environ}
    return dict(env, CODEX_WRAPPER_PORT=str(port), CODEX_WRAPPER_TOKEN=token)
