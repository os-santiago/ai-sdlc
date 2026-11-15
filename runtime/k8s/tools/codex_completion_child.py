"""Internal fixed subprocess launcher; no network endpoint or prompt handling."""
import os
import resource
import sys


if __name__ == "__main__":
    if len(sys.argv) < 2 or not os.path.isabs(sys.argv[1]):
        raise SystemExit(1)
    # Includes CLI-owned cache/log files, not just its final response. The parent
    # applies a tighter accepted-response cap and an independent wall deadline.
    resource.setrlimit(resource.RLIMIT_FSIZE, (8 * 1024 * 1024, 8 * 1024 * 1024))
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    os.execv(sys.argv[1], sys.argv[1:])
