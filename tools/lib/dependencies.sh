#!/bin/bash

# Dependency check utilities for tools scripts
# This ensures required dependencies are available before running tools

# Check and install Python PyYAML module
# This is required by tools/lib/version-utils.sh for YAML parsing
function ensure_pyyaml() {
    if python3 -c "import yaml" &>/dev/null; then
        return 0
    fi

    echo "PyYAML not found. Installing..." >&2

    # Try pip first
    if command -v pip3 &>/dev/null; then
        pip3 install pyyaml --quiet && echo "PyYAML installed successfully via pip3." >&2 && return 0
    elif command -v pip &>/dev/null; then
        pip install pyyaml --quiet && echo "PyYAML installed successfully via pip." >&2 && return 0
    fi

    # Try system package manager
    if command -v dnf &>/dev/null; then
        echo "Using dnf to install python3-pyyaml..." >&2
        sudo dnf install -y python3-pyyaml --quiet
    elif command -v yum &>/dev/null; then
        echo "Using yum to install python3-pyyaml..." >&2
        sudo yum install -y python3-pyyaml --quiet
    elif command -v apt-get &>/dev/null; then
        echo "Using apt-get to install python3-yaml..." >&2
        sudo apt-get update -qq && sudo apt-get install -y python3-yaml --quiet
    else
        echo "Error: No package manager found. Please install PyYAML manually:" >&2
        echo "  - Fedora/RHEL: sudo dnf install python3-pyyaml" >&2
        echo "  - Debian/Ubuntu: sudo apt-get install python3-yaml" >&2
        echo "  - Or use pip: pip3 install pyyaml" >&2
        return 1
    fi

    # Verify installation
    if python3 -c "import yaml" &>/dev/null; then
        echo "PyYAML installed successfully." >&2
        return 0
    else
        echo "Error: PyYAML installation failed." >&2
        return 1
    fi
}

# Silently ensure PyYAML is available
# This is called automatically when sourcing version-utils.sh
ensure_pyyaml || exit 1
