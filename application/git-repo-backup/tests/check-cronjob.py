#!/usr/bin/env python3
"""Validate the security and SSH volume contract of a rendered CronJob."""

import json
import sys
from pathlib import Path


def main() -> int:
    if len(sys.argv) != 3 or sys.argv[2] not in {"0", "1"}:
        print("usage: check-cronjob.py CRONJOB_JSON SSH_ENABLED", file=sys.stderr)
        return 2

    cron = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
    pod = cron["spec"]["jobTemplate"]["spec"]["template"]["spec"]
    ssh_enabled = sys.argv[2] == "1"
    security = pod.get("securityContext", {})
    if security.get("runAsNonRoot") is not True:
        raise SystemExit("pod runAsNonRoot must be true")
    if security.get("runAsUser") != 10001 or security.get("runAsGroup") != 10001:
        raise SystemExit("pod must run as uid/gid 10001")

    init = next(c for c in pod["initContainers"] if c["name"] == "prepare")
    main = next(c for c in pod["containers"] if c["name"] == "git-repo-backup")
    if init.get("securityContext", {}).get("runAsUser") != 0:
        raise SystemExit("prepare initContainer must run as uid 0")
    main_sec = main.get("securityContext", {})
    if main_sec.get("readOnlyRootFilesystem") is not True:
        raise SystemExit("main container must use a read-only root filesystem")
    if main_sec.get("allowPrivilegeEscalation") is not False:
        raise SystemExit("main container must forbid privilege escalation")
    if "ALL" not in main_sec.get("capabilities", {}).get("drop", []):
        raise SystemExit("main container must drop ALL capabilities")

    ssh_volume_names = {"input-ssh", "prepared-ssh", "known-hosts-state"}
    volume_names = {volume["name"] for volume in pod.get("volumes", [])}
    mount_names = {
        mount["name"]
        for container in pod.get("initContainers", []) + pod.get("containers", [])
        for mount in container.get("volumeMounts", [])
    }
    if ssh_enabled:
        if not ssh_volume_names.issubset(volume_names):
            raise SystemExit(
                "SSH-enabled Job must define the SSH key and accept-new state volumes"
            )
    elif ssh_volume_names & (volume_names | mount_names):
        raise SystemExit("HTTPS-only Job must not define or mount SSH volumes")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
