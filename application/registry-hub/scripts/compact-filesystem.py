#!/usr/bin/env python3
"""Reclaim legacy locks and empty directories in an offline backend root."""

import argparse
import fcntl
import json
import os
import stat


LOCK_DIRECTORY = ".registry-hub-locks"


def compact(root, apply=False):
    if not os.path.isabs(root) or os.path.realpath(root) == "/":
        raise ValueError("root must be an absolute backend directory other than /")
    descriptor = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        counts = {"legacyLocks": 0, "emptyDirectories": 0, "applied": apply}
        visit(root, root, os.fstat(descriptor).st_dev, apply, counts)
        return counts
    finally:
        os.close(descriptor)


def visit(directory, root, device, apply, counts):
    empty = True
    with os.scandir(directory) as entries:
        for entry in entries:
            info = entry.stat(follow_symlinks=False)
            if info.st_dev != device or (
                directory == root and entry.name == LOCK_DIRECTORY
            ):
                empty = False
            elif stat.S_ISDIR(info.st_mode):
                if visit(entry.path, root, device, apply, counts):
                    counts["emptyDirectories"] += 1
                    if apply:
                        os.rmdir(entry.path)
                else:
                    empty = False
            elif (
                stat.S_ISREG(info.st_mode)
                and entry.name.endswith(".lock")
                and info.st_size == 0
            ):
                counts["legacyLocks"] += 1
                if apply:
                    os.unlink(entry.path)
            else:
                empty = False
    return empty


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, help="absolute backend storage root")
    parser.add_argument("--apply", action="store_true", help="apply the dry-run plan")
    parser.add_argument(
        "--offline", action="store_true", help="confirm that all backend writers are stopped"
    )
    arguments = parser.parse_args()
    if arguments.apply and not arguments.offline:
        parser.error("--apply requires --offline; stop every backend writer first")
    try:
        print(json.dumps(compact(arguments.root, arguments.apply), sort_keys=True))
    except (OSError, ValueError) as error:
        parser.exit(1, f"compaction failed: {error}\n")


if __name__ == "__main__":
    main()
