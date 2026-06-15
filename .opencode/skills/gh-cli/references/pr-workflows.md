# Pull Request Workflows

Comprehensive guide to creating, reviewing, and merging pull requests with `gh pr`.

## Creating Pull Requests

### Basic creation

```bash
gh pr create --title "Add feature X" --body "Implements feature X as described in #42"
```

Prompts for title and body if not provided.

### Create with all common flags

```bash
gh pr create \
  --title "Fix critical bug" \
  --body "Closes #123\n\nChanges:\n- Fixed parser error\n- Added tests" \
  --base main \
  --head feature/parser-fix \
  --label bug,critical \
  --assignee @me \
  --reviewer john,@team/reviewers \
  --milestone "v2.0" \
  --draft
```

**Flag reference:**
- `--title <text>` — PR title
- `--body <text>` — PR description
- `--base <branch>` — target branch (default: repo default)
- `--head <branch>` — source branch (default: current branch)
- `--label <name>` — add label (repeatable)
- `--assignee <login>` — assign user (repeatable)
- `--reviewer <handle>` — request review from user/team (repeatable)
- `--milestone <name>` — add to milestone
- `--draft` — create as draft
- `--web` — open in browser instead of creating
- `--editor` — open text editor for title + body

### Using templates

Create a PR from a template file:

```bash
# Use a specific template
gh pr create --template pull_request_template.md

# Or with content override
gh pr create --title "Override title" --body-file description.txt
```

### Auto-populate from commits

```bash
# Fill title/body from current branch's commits
gh pr create --fill

# Use first commit only for title/body
gh pr create --fill-first

# Use full commit message + body
gh pr create --fill-verbose
```

### Link issues (auto-close on merge)

Include special keywords in the PR body:

```bash
gh pr create \
  --title "Fix parser" \
  --body "Fixes #42

This PR resolves the parsing issue reported in #42.
Also addresses #99 and #102."
```

**Keywords:** `Fixes`, `Closes`, `Resolves` (case-insensitive)

### Draft PRs (request review before merge)

```bash
# Create as draft
gh pr create --draft

# Later, mark as ready
gh pr ready 42

# Or back to draft
gh pr convert-to-draft 42  # Not a native command; use web UI
```

---

## Viewing & Listing Pull Requests

### List with filters

```bash
# Open PRs (default)
gh pr list

# All states
gh pr list --state all

# Specific branch
gh pr list --head feature/new-ui

# By author
gh pr list --author @me
gh pr list --author john

# By assignee
gh pr list --assignee @me

# By label
gh pr list --label bug
gh pr list --label bug --label critical

# Search
gh pr list --search "is:open author:@me"
```

**Filter flags:**
- `--state <open|closed|merged|all>` — default: open
- `--head <branch>` — filter by source branch
- `--base <branch>` — filter by target branch
- `--author <user>` — filter by creator
- `--assignee <user>` — filter by assignee
- `--label <name>` — filter by label (repeatable)
- `--search <query>` — advanced search syntax
- `--limit <n>` — max results (default: 30)
- `--web` — open list in browser

### View PR details

```bash
# View current branch's PR
gh pr view

# View specific PR
gh pr view 42
gh pr view https://github.com/owner/repo/pull/42

# With comments
gh pr view 42 --comments

# As JSON
gh pr view 42 --json number,title,author,state
```

### Check CI status

```bash
# View checks for a PR
gh pr checks 42

# Watch checks until complete
gh pr checks 42 --watch
```

---

## Reviewing Pull Requests

### Approve a PR

```bash
gh pr review 42 --approve
```

### Request changes

```bash
gh pr review 42 --request-changes --body "Please refactor the loop"
```

### Comment without approval

```bash
gh pr review 42 --comment --body "Great work, one small thing..."
```

### Review current branch's PR

```bash
gh pr review --approve
```

---

## Merging Pull Requests

### Merge strategies

**Merge commit** (default):
```bash
gh pr merge 42
```

**Squash** (combine all commits):
```bash
gh pr merge 42 --squash
```

**Rebase** (apply commits on top of base):
```bash
gh pr merge 42 --rebase
```

### Merge with options

```bash
gh pr merge 42 \
  --squash \
  --delete-branch \
  --subject "Squashed: Fix parser" \
  --body "Merged by CI"
```

**Flag reference:**
- `--squash` — squash commits
- `--rebase` — rebase onto base branch
- `--merge` — merge commit (default)
- `--delete-branch` — delete head branch after merge
- `--subject <text>` — custom merge commit message
- `--body <text>` — merge commit body
- `--auto` — auto-merge when checks pass
- `--admin` — bypass branch protection (if admin)

### Auto-merge (wait for checks)

```bash
# Enable auto-merge; merge when all checks pass
gh pr merge 42 --auto --squash

# Disable auto-merge
gh pr merge 42 --disable-auto
```

---

## Editing Pull Requests

### Edit title and body

```bash
gh pr edit 42 --title "New title"
gh pr edit 42 --body "Updated description"
gh pr edit 42 --body-file description.txt
```

### Edit metadata

```bash
gh pr edit 42 --label bug --label urgent
gh pr edit 42 --assignee alice
gh pr edit 42 --reviewer bob,@team/reviewers
gh pr edit 42 --milestone "v3.0"
```

### Add/remove labels

```bash
# Current labels are replaced
gh pr edit 42 --label bug,critical

# To append, use PR view then re-set all
LABELS=$(gh pr view 42 --json labels --jq '.[].name | join(",")')
gh pr edit 42 --label "$LABELS,new-label"
```

---

## Working with PR Branches

### Check out a PR locally

```bash
gh pr checkout 42
# Switches to the PR's branch locally
```

### View PR diff

```bash
# Show diff for current branch's PR
gh pr diff

# Show diff for specific PR
gh pr diff 42

# Pipe to tools
gh pr diff 42 | less
gh pr diff 42 | git apply  # apply as patch
```

### Update PR branch from base

```bash
# Keep PR branch in sync with base
gh pr update-branch 42

# Or use GitHub web UI
```

---

## Closing & Reopening PRs

### Close a PR

```bash
gh pr close 42
```

### Reopen a closed PR

```bash
gh pr reopen 42
```

### Revert a merged PR (creates new PR)

```bash
gh pr revert 42
# Creates a new PR that reverts the merged PR
```

---

## Locking & Unpinning

### Lock PR conversation (prevent comments)

```bash
gh pr lock 42
```

### Unlock

```bash
gh pr unlock 42
```

---

## Advanced Patterns

### Bulk create PRs from branches

```bash
for branch in feature-{1..5}; do
  gh pr create --base main --head "$branch" --draft
done
```

### List PRs by status using JSON

```bash
# Draft PRs
gh pr list --json number,title,isDraft --jq '.[] | select(.isDraft) | "#\(.number) \(.title)"'

# Awaiting review
gh pr list --json number,title,latestReviews --jq '.[] | select(.latestReviews == []) | "#\(.number) \(.title)"'

# With conflicts
gh pr list --json number,title,mergeable --jq '.[] | select(.mergeable == "CONFLICTING") | "#\(.number) \(.title)"'
```

### Monitor PR for merge readiness

```bash
#!/bin/bash
PR=42
while true; do
  STATUS=$(gh pr view $PR --json mergeable --jq '.mergeable')
  if [ "$STATUS" = "MERGEABLE" ]; then
    echo "PR $PR is ready to merge"
    break
  fi
  echo "PR $PR status: $STATUS. Waiting..."
  sleep 10
done
```

### Create PR with linked issue template

```bash
ISSUE=42
gh pr create \
  --title "Resolve issue #$ISSUE" \
  --body "Closes #$ISSUE" \
  --label automated
```
