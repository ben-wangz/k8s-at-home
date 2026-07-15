#!/bin/bash

set -o errexit -o nounset -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

ARTIFACT_KIND="${1:?artifact kind is required}"
TAG_NAME="${TAG_NAME:?TAG_NAME is required}"
FORGEKIT_BIN="${FORGEKIT_BIN:?FORGEKIT_BIN is required}"
GITHUB_OUTPUT="${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

if [[ ! "${TAG_NAME}" =~ ^([a-z0-9][a-z0-9-]*)-v([0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?)$ ]]; then
    echo "invalid tag format, expected <app-name>-v<semver>" >&2
    exit 1
fi

APP_NAME="${BASH_REMATCH[1]}"

release_json="$(${FORGEKIT_BIN} --output json --project-root "${PROJECT_ROOT}" version get "${APP_NAME}")"

RELEASE_JSON="${release_json}" ARTIFACT_KIND="${ARTIFACT_KIND}" TAG_NAME="${TAG_NAME}" GITHUB_OUTPUT="${GITHUB_OUTPUT}" python3 <<'PY'
import json
import os
import re
import sys


def fail(message: str) -> None:
    print(message, file=sys.stderr)
    raise SystemExit(1)


def write_output(name: str, value: str) -> None:
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as handle:
        handle.write(f"{name}={value}\n")


def write_multiline(name: str, value: str) -> None:
    marker = "__EOF__"
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as handle:
        handle.write(f"{name}<<{marker}\n{value}\n{marker}\n")


tag_name = os.environ["TAG_NAME"].strip()
match = re.fullmatch(r"([a-z0-9][a-z0-9-]*)-v([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?)", tag_name)
if not match:
    fail("invalid tag format, expected <app-name>-v<semver>")

app_name, tag_version = match.groups()
artifact_kind = os.environ["ARTIFACT_KIND"].strip()
payload = json.loads(os.environ["RELEASE_JSON"])
data = payload.get("data", payload)
app = data.get("app")
if not isinstance(app, dict):
    fail("forgekit version get did not return app metadata")

app_type = app.get("type", "")
app_path = app.get("path", "")
app_value = app.get("value", "")
linked = app.get("linked") or []

write_output("app_name", app_name)
write_output("tag_version", tag_version)
write_output("app_type", str(app_type))
write_output("app_path", str(app_path))
write_output("release_name", f"{app_name} v{tag_version}")

should_run = False
chart_dir = ""
matrix = {"include": []}

if artifact_kind == "chart":
    if app_type == "chart":
        if app_value != tag_version:
            fail(f"tag version mismatch for chart {app_name}: tag={tag_version}, forgekit={app_value}")
        should_run = True
        chart_dir = str(app_path)
elif artifact_kind == "container":
    if app_type == "chart":
        include = []
        for item in linked:
            if item.get("type") == "container":
                include.append(
                    {
                        "name": str(item.get("name", "")),
                        "path": str(item.get("path", "")),
                        "value": str(item.get("value", "")),
                    }
                )
        if include:
            should_run = True
            matrix = {"include": include}
    elif app_type == "container":
        if app_value != tag_version:
            fail(f"tag version mismatch for container {app_name}: tag={tag_version}, forgekit={app_value}")
        should_run = True
        matrix = {
            "include": [
                {
                    "name": app_name,
                    "path": str(app_path),
                    "value": str(app_value),
                }
            ]
        }
elif artifact_kind == "binary":
    if app_type == "chart":
        include = []
        for item in linked:
            if item.get("type") == "binary":
                include.append(
                    {
                        "name": str(item.get("name", "")),
                        "path": str(item.get("path", "")),
                        "value": str(item.get("value", "")),
                    }
                )
        if include:
            should_run = True
            matrix = {"include": include}
    elif app_type == "binary":
        if app_value != tag_version:
            fail(f"tag version mismatch for binary {app_name}: tag={tag_version}, forgekit={app_value}")
        should_run = True
        matrix = {
            "include": [
                {
                    "name": app_name,
                    "path": str(app_path),
                    "value": str(app_value),
                }
            ]
        }
else:
    fail(f"unsupported artifact kind: {artifact_kind}")

write_output("should_run", "true" if should_run else "false")
write_output("chart_dir", chart_dir)
write_multiline("matrix_json", json.dumps(matrix, separators=(",", ":")))
PY
