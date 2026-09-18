#!/usr/bin/env python3
"""Render ${NAME} placeholders in a test manifest or values template."""

import re
import sys
from pathlib import Path


def main() -> int:
    if len(sys.argv) < 5 or (len(sys.argv) - 3) % 2 != 0:
        print(
            "usage: render-template.py TEMPLATE OUTPUT NAME VALUE [NAME VALUE ...]",
            file=sys.stderr,
        )
        return 2

    template = Path(sys.argv[1])
    output = Path(sys.argv[2])
    content = template.read_text(encoding="utf-8")
    for name, value in zip(sys.argv[3::2], sys.argv[4::2]):
        content = content.replace("${" + name + "}", value)

    unresolved = sorted(set(re.findall(r"\$\{[A-Z][A-Z0-9_]*\}", content)))
    if unresolved:
        print(
            f"unresolved placeholders in {template}: {', '.join(unresolved)}",
            file=sys.stderr,
        )
        return 1
    output.write_text(content, encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
