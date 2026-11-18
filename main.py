#!/usr/bin/env python3
"""Entrypoint wrapper for the Go forge.

The harness runs `python3 /bot/main.py`. The forge itself is a statically
linked Go binary committed beside this file as ./forge; this wrapper simply
replaces the current process with it via exec, so the trial sees exactly one
process serving the forge (no Python runtime stays resident).
"""

import os
import sys


def main() -> int:
    here = os.path.dirname(os.path.abspath(__file__))
    binary = os.path.join(here, "forge")
    if not os.path.isfile(binary):
        sys.stderr.write("[forge] compiled binary ./forge is missing\n")
        return 1
    if not os.access(binary, os.X_OK):
        # A checkout that dropped the executable bit should not sink the
        # trial: restore it, and only give up if even that fails.
        try:
            os.chmod(binary, 0o755)
        except OSError as exc:
            sys.stderr.write(f"[forge] ./forge is not executable: {exc}\n")
            return 1
    # Replace this process with the forge binary, forwarding argv untouched.
    sys.stdout.flush()
    try:
        os.execv(binary, [binary] + sys.argv[1:])
    except OSError as exc:
        sys.stderr.write(f"[forge] cannot execute ./forge: {exc}\n")
        return 1
    return 1  # unreachable: execv only returns on failure


if __name__ == "__main__":
    sys.exit(main())
