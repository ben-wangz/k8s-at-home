# Output Formatting and JSON Reference

Master JSON output, jq filtering, and Go template formatting with gh.

## JSON Output Basics

### Get JSON output

```bash
# Enable JSON mode with specific fields
gh pr list --json number,title,author

# View available fields for a command
gh pr list --json
```

### Available fields by command

**`gh pr list`:**
`additions`, `assignees`, `author`, `autoMergeRequest`, `baseRefName`, `baseRefOid`, `body`, `changedFiles`, `closed`, `closedAt`, `comments`, `commits`, `createdAt`, `deletions`, `files`, `headRefName`, `headRefOid`, `headRepository`, `isDraft`, `labels`, `latestReviews`, `maintainerCanModify`, `mergeCommit`, `mergeable`, `mergedAt`, `mergedBy`, `milestone`, `number`, `potentialMergeCommit`, `projectCards`, `projectItems`, `reactionGroups`, `reviewDecision`, `state`, `stateReason`, `statusCheckRollup`, `title`, `updatedAt`, `url`

**`gh issue list`:**
`assignees`, `author`, `body`, `closed`, `closedAt`, `comments`, `createdAt`, `id`, `isPinned`, `labels`, `milestone`, `number`, `projectCards`, `projectItems`, `reactionGroups`, `state`, `stateReason`, `title`, `updatedAt`, `url`

**`gh release list`:**
`author`, `createdAt`, `description`, `isDraft`, `isLatest`, `isPrerelease`, `name`, `publishedAt`, `tagName`, `url`

**`gh repo list`:**
`description`, `diskUsage`, `forkCount`, `homepageUrl`, `id`, `isArchived`, `isPrivate`, `isTemplate`, `language`, `name`, `nameWithOwner`, `owner`, `pushedAt`, `stargazerCount`, `updatedAt`, `url`

---

## jq Filtering

Use `--jq` to filter and transform JSON output.

### Basic jq filters

```bash
# Select specific field from each item
gh pr list --json title --jq '.[] | .title'

# Count items
gh pr list --json number --jq 'length'

# Filter by condition
gh issue list --json number,state --jq '.[] | select(.state == "OPEN")'

# Extract and join
gh pr list --json author --jq '.[].author.login'
```

### jq operators

| Operator | Purpose | Example |
|---|---|---|
| `.field` | Access field | `.title` |
| `.[]` | Iterate array | `.[] \| .number` |
| `select(condition)` | Filter | `select(.state == "OPEN")` |
| `map(transform)` | Transform array | `map(.number)` |
| `group_by(field)` | Group items | `group_by(.author.login)` |
| `sort_by(field)` | Sort | `sort_by(.updatedAt)` |
| `reverse` | Reverse | `reverse` |
| `unique_by(field)` | Deduplicate | `unique_by(.author.login)` |
| `length` | Count | `length` |
| `@csv` | CSV format | `.[] \| [.number, .title] \| @csv` |
| `@json` | JSON format | `@json` |

### Common jq patterns

**Get PR titles:**
```bash
gh pr list --json number,title --jq '.[] | "#\(.number) \(.title)"'
```

**Filter issues by label:**
```bash
gh issue list --json number,labels --jq '.[] | select(.labels[].name | contains("bug")) | .number'
```

**Count by author:**
```bash
gh pr list --json author --jq '[.[] | .author.login] | group_by(.) | map({author: .[0], count: length})'
```

**Extract dates:**
```bash
gh pr list --json number,createdAt --jq '.[] | {number: .number, date: (.createdAt | split("T")[0])}'
```

**Export to CSV:**
```bash
gh pr list --json number,title,state --jq -r '.[] | [.number, .title, .state] | @csv'
```

---

## Go Templates

Use `--template` for powerful formatting with Go syntax.

### Template basics

```bash
# Simple iteration
gh pr list --json number,title --template '{{range .}}#{{.number}} {{.title}}{{"\n"}}{{end}}'

# Conditional
gh issue list --json number,state --template '{{range .}}{{if eq .state "OPEN"}}[OPEN] #{{.number}}{{"\n"}}{{end}}{{end}}'

# Format fields
gh release list --json name,publishedAt --template '{{range .}}{{.name}}: {{.publishedAt | truncate 10}}{{"\n"}}{{end}}'
```

### Template functions

**Basic functions:**
- `tablerow <fields>...` — add row to table
- `tablerender` — render accumulated table rows
- `join <sep> <list>` — join with separator
- `pluck <field> <list>` — extract field from items
- `truncate <length> <str>` — limit string length
- `color <style> <str>` — colorize text
- `autocolor <style> <str>` — colorize only in terminal
- `hyperlink <url> <text>` — terminal hyperlink

**Time functions:**
- `timeago <time>` — relative time ("2 days ago")
- `timefmt <format> <time>` — Go time format
- `now` — current time

**Text functions:**
- `contains <substring>` (Sprig) — check inclusion
- `hasPrefix <prefix>` (Sprig) — check prefix
- `hasSuffix <suffix>` (Sprig) — check suffix
- `regexMatch <regex>` (Sprig) — regex match
- `upper` (Sprig) — uppercase
- `lower` (Sprig) — lowercase
- `title` (Sprig) — title case
- `default <value>` (Sprig) — fallback value

### Table output with template

```bash
# Create table with headers and rows
gh pr list \
  --json number,title,state,createdAt \
  --template '{{tablerow "PR" "Title" "State" "Created"}}{{range .}}{{tablerow (printf "#%v" .number) .title .state (timeago .createdAt)}}{{end}}{{tablerender}}'
```

Output:
```
PR   Title              State   Created
#42  Add feature X      OPEN    3 days ago
#41  Fix bug            MERGED  1 week ago
```

### Custom formatting examples

**PRs with status badges:**
```bash
gh pr list --json number,title,state --template '{{range .}}{{if eq .state "OPEN"}}🔵{{else}}✅{{end}} #{{.number}} {{.title}}{{"\n"}}{{end}}'
```

**Issues with age:**
```bash
gh issue list --json number,title,createdAt --template '{{range .}}#{{.number}} {{.title}} ({{timeago .createdAt}}){{"\n"}}{{end}}'
```

**Release notes table:**
```bash
gh release list --json name,author,publishedAt --template '{{tablerow "Version" "Author" "Released"}}{{range .}}{{tablerow .name .author.login (timefmt "2006-01-02" .publishedAt)}}{{end}}{{tablerender}}'
```

**Colored status:**
```bash
gh run list --json status,conclusion --template '{{range .}}{{autocolor "red" .conclusion}} {{.status}}{{"\n"}}{{end}}'
```

---

## Environment Variables Reference

Control gh behavior:

| Variable | Purpose | Example |
|---|---|---|
| `GH_TOKEN` | Authentication token | `export GH_TOKEN=ghp_...` |
| `GH_HOST` | GitHub hostname | `export GH_HOST=github.company.com` |
| `GH_REPO` | Default repo | `export GH_REPO=owner/repo` |
| `GH_EDITOR` | Text editor | `export GH_EDITOR=vim` |
| `GH_BROWSER` | Web browser | `export GH_BROWSER=firefox` |
| `GH_DEBUG` | Verbose mode | `export GH_DEBUG=api` (show HTTP) |
| `GH_PAGER` | Paging program | `export GH_PAGER=less` |
| `GH_CONFIG_DIR` | Config directory | `export GH_CONFIG_DIR=~/.config/gh` |
| `GH_NO_UPDATE_NOTIFIER` | Skip update check | `export GH_NO_UPDATE_NOTIFIER=1` |
| `NO_COLOR` | Disable ANSI colors | `export NO_COLOR=1` |
| `CLICOLOR` | Disable colors (set to 0) | `export CLICOLOR=0` |
| `CLICOLOR_FORCE` | Force colors when piped | `export CLICOLOR_FORCE=1` |
| `GH_COLOR_LABELS` | Show label colors | `export GH_COLOR_LABELS=1` |
| `GH_FORCE_TTY` | Force terminal output | `export GH_FORCE_TTY=1` |
| `GLAMOUR_STYLE` | Markdown style | `export GLAMOUR_STYLE=dark` |
| `GH_ACCESSIBLE_PROMPTER` | Accessible prompts | `export GH_ACCESSIBLE_PROMPTER=1` |

---

## Combining Flags

### JSON + jq together

```bash
# Extract and filter
gh pr list --json number,title,author --jq '.[] | select(.author.login == "alice") | "#\(.number) \(.title)"'

# Complex transformation
gh issue list --json number,state,assignees --jq '.[] | {number, state, count: (.assignees | length)}'
```

### JSON + template together

```bash
# Format as table
gh pr list --json number,title,state,updatedAt --template '{{range .}}{{tablerow "#\(.number)" .title .state (timeago .updatedAt)}}{{end}}{{tablerender}}'
```

### Piping to other tools

```bash
# Export to CSV
gh pr list --json number,title,state --jq -r '.[] | [.number, .title, .state] | @csv' > prs.csv

# Import to Excel via JSON
gh pr list --json number,title,state | jq > prs.json

# Filter and count
gh issue list --json labels --jq '.[].labels[].name' | sort | uniq -c

# Send to Slack webhook
gh pr list --json number,title --template '{{range .}}#{{.number}}: {{.title}}{{"\n"}}{{end}}' | \
  curl -X POST -d "text=$(cat)" https://hooks.slack.com/...
```

---

## Pagination with JSON

### Fetch all pages

```bash
# Get all results across pages
gh pr list --limit 1000 --json number
# or
gh api repos/{owner}/{repo}/issues --paginate
```

### Paginate with jq

```bash
# Combine all pages and filter
gh pr list --limit 1000 --json number,title,author --jq '.[] | select(.author.login == "bob")'
```

### Slurp all results into single array

```bash
# With GraphQL paginate
gh api graphql --paginate --slurp -f query='...'
```

---

## Real-World Examples

### Dashboard: Recent activity

```bash
#!/bin/bash
echo "=== Open PRs ==="
gh pr list --state open --json number,title,createdAt \
  --template '{{range .}}#{{.number}} {{.title}} ({{timeago .createdAt}}){{"\n"}}{{end}}'

echo -e "\n=== Recent Issues ==="
gh issue list --state open --limit 5 --json number,title \
  --template '{{range .}}#{{.number}} {{.title}}{{"\n"}}{{end}}'

echo -e "\n=== Workflow Status ==="
gh run list --limit 1 --json status,conclusion \
  --template '{{range .}}Status: {{.status}}, Conclusion: {{.conclusion}}{{"\n"}}{{end}}'
```

### Report: Issues by label

```bash
gh issue list --state all --json number,labels \
  --jq '[.[] | .labels[].name] | group_by(.) | map({label: .[0], count: length}) | sort_by(-.count)[]'
```

### Script: Bulk close old issues

```bash
#!/bin/bash
CUTOFF_DATE=$(date -u -d '30 days ago' +%Y-%m-%dT%H:%M:%SZ)
gh issue list --state open --json number,createdAt \
  --jq ".[] | select(.createdAt < \"$CUTOFF_DATE\") | .number" | \
  while read issue; do
    gh issue comment $issue --body "Closing inactive issue"
    gh issue close $issue
  done
```

### Script: Release notes from PRs

```bash
#!/bin/bash
gh pr list --state merged --limit 50 --search "merged:>2026-03-01" \
  --json number,title,author \
  --jq '.[] | "- \(.title) (#\(.number)) by @\(.author.login)"'
```
