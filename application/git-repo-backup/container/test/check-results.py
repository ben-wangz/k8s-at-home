#!/usr/bin/env python3
"""Verify `go test -json` output for the git-repo-backup integration suite.

The checker refuses to call a run successful unless every discovered
top-level test completed with a pass terminal state, no test or subtest
failed or skipped, the package itself passed, and the preserved go test
exit code is zero. Truncated output, empty results, compile failures, and
malformed JSON lines all fail. Subtests are never summed into top-level
counts.

Exit status: 0 only for a fully successful run; 1 otherwise (including
checker input errors).
"""

import argparse
import json
import sys

TERMINALS = ("pass", "fail", "skip")


def load_expected(path):
    """Read top-level test names produced by `go test -list`."""
    try:
        with open(path, "r", encoding="utf-8") as handle:
            return [line.strip() for line in handle if line.strip().startswith("Test")]
    except OSError as exc:
        raise SystemExit(f"check-results: cannot read list file: {exc}")


def parse_events(path):
    """Parse JSON lines; raise ValueError on malformed input."""
    events = []
    try:
        with open(path, "r", encoding="utf-8") as handle:
            for lineno, line in enumerate(handle, 1):
                line = line.strip()
                if not line:
                    continue
                try:
                    events.append(json.loads(line))
                except json.JSONDecodeError as exc:
                    raise ValueError(f"invalid JSON at line {lineno}: {exc}")
    except OSError as exc:
        raise ValueError(f"cannot read events file: {exc}")
    return events


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=("local", "all"), required=True)
    parser.add_argument("--list", required=True, help="go test -list output file")
    parser.add_argument("--events", required=True, help="go test -json output file")
    parser.add_argument("--go-exit", type=int, required=True)
    parser.add_argument("--output", help="result.json path; summary goes to stderr")
    args = parser.parse_args()

    reasons = []
    expected = load_expected(args.list)
    if args.mode == "local":
        expected = [name for name in expected if name.startswith("TestLocal")]
    if not expected:
        reasons.append("no expected tests discovered from list file")

    try:
        events = parse_events(args.events)
    except ValueError as exc:
        events = []
        reasons.append(str(exc))

    started = {}  # test name (including subtests) -> terminal action or None
    package_pass = False
    package_fail = False
    for event in events:
        action = event.get("Action", "")
        test = event.get("Test", "")
        if not isinstance(action, str) or not isinstance(test, str):
            reasons.append("event with non-string Action/Test field")
            continue
        if action == "run" and test:
            started.setdefault(test, None)
        elif action in TERMINALS and test:
            current = started.setdefault(test, None)
            # Worst terminal wins: a fail/skip after a pass still fails.
            if current in TERMINALS:
                if action == "fail" or (action == "skip" and current != "fail"):
                    started[test] = action
            else:
                started[test] = action
        elif action in ("pass", "fail") and not test:
            if action == "pass":
                package_pass = True
            else:
                package_fail = True

    for name, terminal in started.items():
        if terminal is None:
            reasons.append(f"missing terminal state: {name}")
        elif terminal == "fail":
            reasons.append(f"failed: {name}")
        elif terminal == "skip":
            reasons.append(f"skipped: {name}")

    top_names = sorted({name.split("/", 1)[0] for name in started})
    if not started:
        reasons.append("no tests started (empty or truncated output)")
    for name in expected:
        if name not in started:
            reasons.append(f"expected test did not run: {name}")
    if args.mode == "local":
        for name in top_names:
            if not name.startswith("TestLocal"):
                reasons.append(f"out-of-scope test ran in local mode: {name}")
    elif args.mode == "all":
        has_local = any(name.startswith("TestLocal") for name in top_names)
        has_s3 = any(name.startswith("TestS3") for name in top_names)
        if not has_local:
            reasons.append("no TestLocal test ran")
        if not has_s3:
            reasons.append("no TestS3 test ran")
    if not package_pass:
        reasons.append("package pass event missing")
    if package_fail:
        reasons.append("package reported failure")
    if args.go_exit != 0:
        reasons.append(f"go test exit code {args.go_exit}")

    passed = sum(1 for name in top_names if started.get(name) == "pass")
    failed = sum(1 for name in top_names if started.get(name) == "fail")
    skipped = sum(1 for name in top_names if started.get(name) == "skip")
    result = {
        "mode": args.mode,
        "goExitCode": args.go_exit,
        "expectedCount": len(expected),
        "passed": passed,
        "failed": failed,
        "skipped": skipped,
        "status": "pass" if not reasons else "fail",
        "reasons": reasons,
    }
    payload = json.dumps(result, indent=2)
    if args.output:
        with open(args.output, "w", encoding="utf-8") as handle:
            handle.write(payload + "\n")
    print(payload, file=sys.stderr)
    if result["status"] != "pass":
        sys.exit(1)
    sys.exit(0)


if __name__ == "__main__":
    main()
