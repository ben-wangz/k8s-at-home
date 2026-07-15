# Repository and Release Management

Manage repositories, forks, and releases with `gh repo` and `gh release`.

## Repository Operations

### Clone a repository

```bash
# Clone by owner/repo
gh repo clone owner/repo

# Clone to custom directory
gh repo clone owner/repo custom-dir

# Clone and set up
gh repo clone owner/repo
cd repo
```

### Create a new repository

```bash
# Interactive creation
gh repo create

# Create with flags
gh repo create my-project \
  --public \
  --description "My awesome project" \
  --homepage "https://example.com" \
  --source=. \
  --remote origin \
  --push
```

**Flags:**
- `--public` / `--private` — visibility (default: private)
- `--description <text>` — repo description
- `--homepage <url>` — homepage URL
- `--gitignore <template>` — initialize with gitignore
- `--license <license>` — add LICENSE file
- `--source <path>` — source directory (current dir to initialize)
- `--remote <name>` — git remote name (default: origin)
- `--push` — push existing commits
- `--template <repo>` — use repo template

### Fork a repository

```bash
# Fork upstream repo
gh repo fork owner/upstream

# Fork to specific organization
gh repo fork owner/repo --org myorg

# Fork and clone
gh repo fork owner/repo --clone
```

### View repository details

```bash
# View current repo
gh repo view

# View specific repo
gh repo view owner/repo

# With full details
gh repo view owner/repo --json \
  description,homepageUrl,isPrivate,stargazerCount,forkCount

# Open in browser
gh repo view owner/repo --web
```

### Edit repository settings

```bash
# Update description
gh repo edit --description "New description"

# Make private
gh repo edit --private

# Make public
gh repo edit --public

# Update homepage
gh repo edit --homepage "https://newsite.com"

# Update default branch
gh repo edit --default-branch main
```

### Sync a fork

```bash
# Update fork from upstream
gh repo sync

# Specify upstream
gh repo sync --source owner/upstream

# Delete branch after syncing
gh repo sync --delete-branch
```

### Archive a repository

```bash
# Archive repo (make read-only)
gh repo archive owner/repo

# Unarchive
gh repo unarchive owner/repo
```

### Delete a repository

```bash
# Delete repo (requires confirmation)
gh repo delete owner/repo

# Force delete (skip confirmation)
gh repo delete owner/repo --confirm
```

---

## Working with .gitignore Templates

### List available templates

```bash
gh repo gitignore list | head -20
# Output: Node, Python, Ruby, Go, Java, etc.
```

### View a template

```bash
gh repo gitignore Node
# Shows Node.js .gitignore content
```

### Initialize repo with template

```bash
gh repo create my-project --gitignore Node
```

---

## Licenses

### List available licenses

```bash
gh repo license list
```

### View license

```bash
gh repo license MIT
```

### Add license to repo

```bash
gh repo create my-project --license MIT
```

---

## Deploy Keys

Manage deployment keys for CI/CD:

```bash
# List deploy keys
gh repo deploy-key list

# Add a deploy key
gh repo deploy-key add id_rsa.pub --title "CI deployment"

# Delete deploy key
gh repo deploy-key delete <key-id>
```

---

## Autolink References

Create custom URL patterns for issue references:

```bash
# List autolinks
gh repo autolink list

# Add autolink (example: JIRA integration)
gh repo autolink add --key-prefix JIRA --url-template "https://jira.company.com/browse/JIRA-<num>"

# Delete autolink
gh repo autolink delete JIRA
```

---

## Release Management

### Create a release

```bash
# Simple release
gh release create v1.0.0

# With notes
gh release create v1.0.0 --notes "First stable release"

# With title and notes
gh release create v1.0.0 \
  --title "Version 1.0 - Stable" \
  --notes "Ready for production"

# Create from file
gh release create v1.0.0 --notes-file CHANGELOG.md
```

**Flags:**
- `--notes <text>` — release notes
- `--notes-file <file>` — read notes from file
- `--title <text>` — release title
- `--draft` — create as draft
- `--prerelease` — mark as pre-release
- `--target <branch>` — git ref for tag (default: main/master)
- `--discussion-category <category>` — enable discussions

### Upload assets to release

```bash
# Upload single file
gh release upload v1.0.0 dist/app-1.0.0.tar.gz

# Upload multiple files
gh release upload v1.0.0 dist/*.tar.gz dist/*.zip

# Overwrite existing asset
gh release upload v1.0.0 dist/app.zip --clobber
```

### Edit a release

```bash
# Update notes
gh release edit v1.0.0 --notes "Updated release notes"

# Change draft status
gh release edit v1.0.0 --draft=false
```

### List releases

```bash
# List all releases
gh release list

# Limit results
gh release list --limit 10
```

### View release details

```bash
# View release info
gh release view v1.0.0

# With download links
gh release view v1.0.0 --json assets
```

### Download release assets

```bash
# Download all assets from release
gh release download v1.0.0

# Download to specific directory
gh release download v1.0.0 --dir ~/Downloads

# Download specific asset
gh release download v1.0.0 --pattern "*.tar.gz"

# Skip existing files
gh release download v1.0.0 --skip-existing
```

### Delete release

```bash
# Delete a release
gh release delete v1.0.0

# Force delete
gh release delete v1.0.0 --yes
```

---

## Release Attestations

Sign releases with attestations (requires provenanceFile):

```bash
# Create release with provenance
gh release create v1.0.0 --notes "Signed release"

# Verify attestation
gh release verify v1.0.0

# Verify specific asset attestation
gh release verify-asset dist/app.zip
```

---

## Advanced Patterns

### Automate release from tag

```bash
#!/bin/bash
# Create release from existing git tag

TAG=v1.0.0
CHANGELOG=$(cat CHANGELOG.md)

gh release create $TAG \
  --title "Release $TAG" \
  --notes "$CHANGELOG" \
  dist/*.tar.gz dist/*.zip
```

### Bulk upload assets

```bash
#!/bin/bash
# Upload multiple builds for different platforms

VERSION=1.0.0
for file in build/release/*; do
  gh release upload "v$VERSION" "$file"
  echo "Uploaded: $(basename $file)"
done
```

### Create release from draft

```bash
# Create as draft first
gh release create v1.0.0 --draft --notes "Work in progress"

# Add assets while draft
gh release upload v1.0.0 dist/app.zip

# Publish
gh release edit v1.0.0 --draft=false
```

### Monitor release downloads

```bash
#!/bin/bash
# Track release download statistics

VERSION=v1.0.0
while true; do
  echo "=== $(date) ==="
  gh release view $VERSION --json assets \
    --jq '.assets | map({name: .name, downloads: .downloadCount})'
  sleep 3600
done
```

### Generate release notes from PRs

```bash
#!/bin/bash
# Create release notes from merged PRs since last version

LAST_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "HEAD~100")
CURRENT_TAG=${1:-v$(date +%Y.%m.%d)}

echo "## Changes since $LAST_TAG" > RELEASE_NOTES.md
echo "" >> RELEASE_NOTES.md
gh pr list \
  --state merged \
  --json title,number,author \
  --jq '.[] | "- \(.title) (#\(.number)) by @\(.author.login)"' \
  >> RELEASE_NOTES.md

gh release create $CURRENT_TAG --notes-file RELEASE_NOTES.md
```

### Set default license for new repos

```bash
# When creating repos, always add MIT license
gh repo create my-project --license MIT
```

---

## Set Default Repository

For a directory, set the default repo for gh commands:

```bash
gh repo set-default owner/repo

# Verify
gh repo view
```

Now `gh` commands will use this repo without `-R` flag.
