# Search and API Queries

Master searching GitHub and making direct API calls with `gh search` and `gh api`.

## Searching

### Search issues and pull requests

```bash
# Search open issues with label
gh search issues "is:open label:bug"

# Search PRs by author
gh search prs "author:@me state:open"

# Search for issues mentioning specific text
gh search issues "parser error is:open"

# Closed issues from last month
gh search issues "is:closed closed:>2026-03-01"
```

**Common search qualifiers:**
- `is:open` / `is:closed` — state
- `state:open` / `state:closed` / `state:merged` — PR state
- `author:<user>` — creator
- `assignee:<user>` — assigned to
- `label:<label>` — has label
- `milestone:<name>` — in milestone
- `created:>DATE` / `updated:>DATE` — date filters
- `involves:<user>` — mentioned or involved
- `is:draft` — draft PR
- `is:private` / `is:public` — visibility
- `archived:false` — exclude archived repos

### Search code

```bash
# Find function definition
gh search code "function Parser" --language javascript

# Search in specific repo
gh search code "TODO" repo:owner/repo

# Match exact phrase
gh search code '"exact string"'
```

**Code search flags:**
- `--language <lang>` — filter by language
- `--match <match>` — match type (file, path, etc.)

### Search repositories

```bash
# Stars > 1000
gh search repos "stars:>1000 language:go"

# Recently updated
gh search repos "updated:>2026-03-01 topic:kubernetes"

# User's repos
gh search repos "user:alice sort:stars"
```

**Repo qualifiers:**
- `stars:<n>` — minimum stars
- `forks:<n>` — minimum forks
- `language:<lang>` — primary language
- `topic:<topic>` — has topic tag
- `created:>DATE` — creation date
- `user:<user>` — owned by user
- `org:<org>` — owned by organization
- `is:public` / `is:private` — visibility
- `sort:stars` / `sort:forks` / `sort:updated` — order
- `archived:false` — exclude archived

### Search commits

```bash
# Commits by author
gh search commits "author:alice main"

# Commits touching specific file
gh search commits "path:src/parser.ts"

# Commits in date range
gh search commits "is:merged after:2026-01-01 before:2026-04-01"
```

### JSON output from search

```bash
# Get search results as JSON
gh search issues "is:open" --json number,title,author

# Count results
gh search prs "author:@me state:merged" --json number | jq 'length'

# Extract author logins
gh search issues "is:open" --json author --jq '.[].author.login'
```

---

## GitHub API Queries

### REST API calls

```bash
# List issues in current repo
gh api repos/{owner}/{repo}/issues

# Get specific issue
gh api repos/{owner}/{repo}/issues/42

# Create issue via API
gh api repos/{owner}/{repo}/issues \
  -f title="API Issue" \
  -f body="Created via API"

# Update PR
gh api repos/{owner}/{repo}/pulls/42 \
  -X PATCH \
  -f state=closed
```

**HTTP methods:**
- `-X GET` (default) — fetch data
- `-X POST` — create
- `-X PATCH` — update
- `-X PUT` — replace
- `-X DELETE` — delete

### Field syntax

**String fields:**
```bash
-f field="value"
```

**Typed fields** (`-F` auto-converts scalars):
```bash
-F number=42           # integer
-F active=true         # boolean
```

> **Note:** `-F` auto-converts integers and booleans. It does **not** reliably
> convert JSON arrays or nested objects — the value is sent as a string literal,
> causing HTTP 422 on endpoints that expect a real JSON array. Use `--input`
> for complex payloads (see below).

**Fields from file:**
```bash
-f content=@file.txt
-f data=@data.json
```

**Fields from stdin:**
```bash
cat data.json | gh api repos/{owner}/{repo}/issues -F data=@-
```

### Complex payloads (arrays / nested objects)

When an endpoint expects an array or nested object (e.g., repository topics, label
lists, branch protection rules), **do not use `-F`**. Use `--input` with a JSON file:

```bash
# Write the payload
cat > /tmp/payload.json << 'EOF'
{
  "names": ["opencode", "odoo", "ai-coding"]
}
EOF

# Send it — works reliably for all endpoints
gh api -X PUT repos/{owner}/{repo}/topics --input /tmp/payload.json
```

**Why `--input` instead of `-F names='["a","b"]'`:**
The `-F` flag sends the string `["a","b"]` literally — the API receives a string,
not a JSON array, and returns HTTP 422: *"not an array"*. The `--input` flag sends
the file content as the raw JSON body, bypassing all client-side type coercion.

**Common endpoints that need `--input`:**
- `PUT /repos/{owner}/{repo}/topics` — repository topics (`names` field is an array)
- `PUT /repos/{owner}/{repo}/branches/{branch}/protection` — branch protection rules
- Any endpoint where the GitHub API docs show a field typed as `array` or `object`

### List users in organization

```bash
gh api orgs/myorg/members
```

### Get collaborators for a repo

```bash
gh api repos/{owner}/{repo}/collaborators
```

### Paginate results

```bash
# Fetch all pages
gh api repos/{owner}/{repo}/issues --paginate

# Limit to 100 items per page
gh api repos/{owner}/{repo}/issues?per_page=100 --paginate
```

### Add custom headers

```bash
# Accept preview API
gh api repos/{owner}/{repo}/issues \
  -H "Accept: application/vnd.github.preview+json"

# Cache for 1 hour
gh api repos/{owner}/{repo}/issues \
  --cache 3600s
```

---

## GraphQL Queries

Use GitHub's GraphQL API for complex queries:

```bash
gh api graphql -f owner='{owner}' -f name='{repo}' -f query='
  query($name: String!, $owner: String!) {
    repository(owner: $owner, name: $name) {
      issues(first: 10, states: OPEN) {
        nodes {
          number
          title
          author {
            login
          }
        }
      }
    }
  }
'
```

### Common GraphQL queries

**Get PR and review status:**
```bash
gh api graphql -f owner='{owner}' -f name='{repo}' -f query='
  query($name: String!, $owner: String!) {
    repository(owner: $owner, name: $name) {
      pullRequests(first: 10, states: OPEN) {
        nodes {
          number
          title
          reviews(first: 5) {
            nodes {
              state
              author {
                login
              }
            }
          }
        }
      }
    }
  }
'
```

**List all workflows and recent runs:**
```bash
gh api graphql -f owner='{owner}' -f name='{repo}' -f query='
  query($name: String!, $owner: String!) {
    repository(owner: $owner, name: $name) {
      workflows: object(expression: "HEAD:.github/workflows") {
        ... on Tree {
          entries {
            name
          }
        }
      }
    }
  }
'
```

### Paginate GraphQL results

```bash
gh api graphql --paginate -f query='
  query($endCursor: String) {
    viewer {
      repositories(first: 100, after: $endCursor) {
        nodes {
          nameWithOwner
        }
        pageInfo {
          hasNextPage
          endCursor
        }
      }
    }
  }
'
```

Use `--slurp` to combine pages into single JSON array:

```bash
gh api graphql --paginate --slurp -f query='...'
```

---

## Advanced Patterns

### Export search results to CSV

```bash
gh search issues "is:open" --json number,title,author,createdAt \
  --jq -r '.[] | [.number, .title, .author.login, .createdAt] | @csv'
```

### Monitor issue activity

```bash
#!/bin/bash
# Watch open issues for updates

while true; do
  COUNT=$(gh search issues "is:open" --json number | jq 'length')
  echo "$(date): $COUNT open issues"
  sleep 300
done
```

### Bulk operations via API

```bash
#!/bin/bash
# Close all issues with label "wontfix"

gh search issues "is:open label:wontfix" --json number --jq '.[].number' | while read issue; do
  gh api repos/{owner}/{repo}/issues/$issue \
    -X PATCH \
    -f state=closed \
    -f state_reason=not_planned
done
```

### Get statistics across repos

```bash
# Count stars across user's repos
gh api graphql --paginate --slurp -f query='
  query($endCursor: String) {
    viewer {
      repositories(first: 100, after: $endCursor) {
        nodes {
          nameWithOwner
          stargazerCount
        }
        pageInfo {
          hasNextPage
          endCursor
        }
      }
    }
  }
' | jq '[.[].data.viewer.repositories.nodes[] | .stargazerCount] | add'
```

### Find duplicate issues

```bash
#!/bin/bash
# Find issues with similar titles

gh issue list --state all --json title --jq '.[] | .title' | sort | uniq -d
```

### Track issue velocity

```bash
#!/bin/bash
# Count closed issues per day for last month

MONTH_AGO=$(date -u -d '30 days ago' +%Y-%m-%dT00:00:00Z)
gh search issues "is:closed closed:>$MONTH_AGO" --json closedAt \
  --jq '.[] | .closedAt | split("T")[0]' | sort | uniq -c
```

### List issues by milestone

```bash
gh api repos/{owner}/{repo}/milestones --jq '.[] | .title' | while read milestone; do
  COUNT=$(gh search issues "is:open milestone:\"$milestone\"" --json number | jq 'length')
  echo "$milestone: $COUNT open issues"
done
```
