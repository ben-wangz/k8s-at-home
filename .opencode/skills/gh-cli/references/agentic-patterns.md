# Agentic Output Patterns

Token-efficient patterns for using gh in agent contexts.

## Why Token Efficiency Matters

Every gh command result is embedded in the agent's context window. Large outputs consume tokens that could be used for reasoning. In resource-constrained scenarios, bloated output = reduced decision-making capacity.

Inefficient: `gh pr list --json` → ~400+ chars per PR with nested author object
Efficient: `gh pr list --json number,title,state --jq '.[]' | ...` → ~80 chars per PR

**Rule: Minimize output at the source, before it enters context.**

---

## Environment Setup for Agents

Always set these when invoking gh from an agent:

```bash
export NO_COLOR=1               # Strip ANSI escape codes (\033[...) — saves ~5-10% per line
export GH_NO_UPDATE_NOTIFIER=1  # Suppress "gh has a new version" banner — saves 2-3 lines
export GH_PAGER=cat             # Disable less paging prompts — saves 1-2 lines per 30-line output
```

**Impact:** Eliminates ~5-15% noise tokens per command execution.

Inline usage:

```bash
NO_COLOR=1 GH_NO_UPDATE_NOTIFIER=1 GH_PAGER=cat gh pr list
```

Or in agent init:

```bash
for var in NO_COLOR GH_NO_UPDATE_NOTIFIER GH_PAGER; do
  export $var  # Set before running any gh command
done
GH_PAGER=cat
```

---

## Output Mode Decision Tree

**Choose the right form for each task:**

```
┌─ Do you only need to check existence or count?
│  └─ YES: Use `--json number --jq 'length'` or text form
│          Example: gh pr list --head $(git branch --show-current) --json number --jq 'length > 0'
│
├─ Do you need exact field values (to branch on)?
│  └─ YES: Use `--json <minimal-fields> --jq <filter>`
│          Example: gh pr view 42 --json mergeable --jq '.mergeable'
│
└─ Are you just displaying a list to the user (human readable)?
   └─ YES: Use text form (default output)
           Example: gh pr list
```

### Text Output (Default)

**When to use:**
- Listing for human consumption
- Status checks where you don't branch on fields
- Existence checks (just see if anything returns)

**Output:** Tab-separated, ~80 chars per line

```bash
gh pr list
gh issue list --state open
gh run list --workflow ci.yml
```

### JSON + jq (Agentic)

**When to use:**
- Extracting specific fields for decision-making
- Transforming lists into simple formats
- Filtering by field value

**Output:** Compact, field-projected, typically 20-80 chars per item

```bash
gh pr list --json number,title,state --jq '.[] | "\(.number): \(.title)"'
gh issue list --json assignees --jq '.[] | select(.assignees | length > 0)'
gh release list --json tagName,isLatest --jq 'first.tagName'
```

### Raw JSON (Avoid)

**When NOT to use:**
- `gh pr list --json` alone without `--jq` → produces bloated output
- `gh api` without field projection → returns all fields of all objects
- Any command where you're not using most returned fields

**Impact:** 3-5x more tokens than the jq-filtered equivalent

```bash
# BAD: ~400 chars per PR with id, is_bot, nested author objects
gh pr list --json

# GOOD: ~50 chars per PR with only needed fields
gh pr list --json number,title --jq '.[]'
```

---

## Minimal Field Catalog

Reference table: task → minimal `--json` fields → `--jq` filter (if needed)

### Pull Requests

| Task | Command | Output size |
|---|---|---|
| Count open PRs | `gh pr list --json number --jq 'length'` | 1 number |
| List PRs (text) | `gh pr list` | ~80 chars/line |
| List PRs (data) | `gh pr list --json number,title,state` | ~50 chars per PR |
| PR author | `gh pr list --json author --jq '.[] \| .author.login'` | ~15 chars per PR |
| PR is draft? | `gh pr view 42 --json isDraft --jq '.isDraft'` | `true`/`false` |
| Merge status | `gh pr view 42 --json mergeable,reviewDecision` | ~30 chars total |
| PR labels | `gh pr list --json labels --jq '.[].labels[].name'` | ~20 chars per label |
| CI status | `gh pr checks 42 --json status --jq '.[] \| .status'` | 1 word per check |

### Issues

| Task | Command | Output size |
|---|---|---|
| Count open issues | `gh issue list --json number --jq 'length'` | 1 number |
| List issues (text) | `gh issue list` | ~80 chars/line |
| List issues (data) | `gh issue list --json number,state` | ~30 chars per issue |
| Issue assignees | `gh issue list --json assignees --jq '.[] \| .assignees[].login'` | ~15 chars per assignee |
| Pinned issues | `gh issue list --json number,isPinned --jq '.[] \| select(.isPinned)'` | ~15 chars per pinned |
| Issue labels | `gh issue list --json labels --jq '.[].labels[].name'` | ~20 chars per label |

### Releases

| Task | Command | Output size |
|---|---|---|
| Latest release tag | `gh release list --json tagName,isLatest --jq 'map(select(.isLatest))[0].tagName'` | ~10 chars |
| Release exists? | `gh release list --json tagName --jq 'map(.tagName) \| contains(["v1.0.0"])'` | `true`/`false` |
| Release dates | `gh release list --json tagName,publishedAt --jq '.[] \| {tag: .tagName, date: .publishedAt}'` | ~40 chars per release |

### Repositories

| Task | Command | Output size |
|---|---|---|
| Repo exists? | `gh repo view owner/repo --json id --jq '.id != null'` | `true`/`false` |
| Repo is private? | `gh repo view --json isPrivate --jq '.isPrivate'` | `true`/`false` |
| Star count | `gh repo view owner/repo --json stargazerCount --jq '.stargazerCount'` | 1-5 digits |
| Language | `gh repo view owner/repo --json language --jq '.language'` | ~10 chars |

### GitHub Actions

| Task | Command | Output size |
|---|---|---|
| Latest run status | `gh run list --limit 1 --json status,conclusion --jq '.[0] \| .status'` | 1 word |
| Workflow enabled? | `gh workflow list --json name,state --jq '.[] \| select(.name == "ci.yml") \| .state'` | `enabled`/`disabled` |
| Run artifacts | `gh run view 12345 --json artifacts --jq '.artifacts[].name'` | ~30 chars per artifact |

---

## Bounding Large Outputs

These commands can emit megabytes without guardrails:

### `gh run view --log` (CI logs can be 100MB+)

```bash
# BAD: loads entire CI log into context
gh run view 12345 --log

# GOOD: first 50 lines only
gh run view 12345 --log | head -50

# GOOD: search for error pattern
gh run view 12345 --log | grep -i "error\|fail" | head -20

# GOOD: last 30 lines (usually most relevant)
gh run view 12345 --log | tail -30
```

### `gh pr diff` (large diffs can be 1MB+)

```bash
# BAD: loads entire diff into context
gh pr diff 42

# GOOD: first 100 lines
gh pr diff 42 | head -100

# GOOD: summary only (with --stat flag)
gh pr diff 42 --stat

# GOOD: specific file only
gh pr diff 42 -- src/parser.ts | head -50
```

### `gh api --paginate` (can fetch thousands of objects)

```bash
# BAD: fetches all pages, all fields
gh api repos/{owner}/{repo}/issues --paginate

# GOOD: limit results with jq
gh api repos/{owner}/{repo}/issues --jq '.[0:10]'

# GOOD: field-project each object
gh api repos/{owner}/{repo}/issues --jq '.[] | {number, state}'

# GOOD: stop after finding match
gh api repos/{owner}/{repo}/issues --paginate --jq '.[] | select(.state == "open") | .number' | head -1
```

### `gh pr view --comments` (large discussions)

```bash
# BAD: loads all 500 comments
gh pr view 42 --json comments --jq '.comments'

# GOOD: last 5 comments only
gh pr view 42 --json comments --jq '.comments[-5:]'

# GOOD: count comments
gh pr view 42 --json comments --jq '.comments | length'

# GOOD: latest comment author
gh pr view 42 --json comments --jq '.comments[-1].author.login'
```

---

## GraphQL for Bulk Data

When you need to fetch many objects (100+), **prefer GraphQL with explicit field selection** over REST `--paginate`.

### REST --paginate (inefficient for bulk)

```bash
# Fetches all fields of all issues (megabytes)
gh api repos/{owner}/{repo}/issues --paginate

# With field projection (better, but still REST)
gh api repos/{owner}/{repo}/issues --paginate --jq '.[] | {number, state}'
```

### GraphQL (efficient for bulk)

```bash
# Only fetch fields you need; GraphQL does server-side filtering
gh api graphql -f owner='{owner}' -f name='{repo}' -f query='
  query($name: String!, $owner: String!, $endCursor: String) {
    repository(owner: $owner, name: $name) {
      issues(first: 100, after: $endCursor, states: OPEN) {
        nodes {
          number
          title
          state
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

**Why GraphQL wins:**
- Specify exactly which fields → no wasted bandwidth
- Request only what you need per page
- Structured, not tab-separated
- Server-side filtering possible (`states: OPEN`)

### Pattern: Bulk fetch with pagination

```bash
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
' | jq '.[] | .data.viewer.repositories.nodes[] | select(.stargazerCount > 1000)'
```

---

## jq Patterns for Context Reduction

Use these patterns to shrink output before it enters agent context:

### Extract Single Field

```bash
# Don't: gh pr list --json author (returns full author object)
# Do:
gh pr list --json author --jq '.[].author.login'

# Output: alice
#         bob
#         charlie
```

### Filter Array to First/Last

```bash
# Don't: gh release list (returns all releases)
# Do:
gh release list --json tagName,isLatest --jq 'first'
# or
gh release list --json tagName --jq 'last'

# Output: (single object)
```

### Select with Condition

```bash
# Don't: gh pr list --json (returns all PRs including closed)
# Do:
gh pr list --json state --jq 'select(.state == "OPEN")'

# Filters to only open items
```

### Map to New Structure

```bash
# Don't: gh issue list --json (returns many fields)
# Do:
gh issue list --json number,title --jq 'map({id: .number, desc: .title})'

# Output: compact key-value pairs
```

### Group and Aggregate

```bash
# Count items per author
gh pr list --json author --jq '
  map(.author.login) | 
  group_by(.) | 
  map({author: .[0], count: length})'

# Output: [{author: "alice", count: 3}, ...]
```

### Reduce to Scalar

```bash
# Don't: gh issue list --json (entire list in context)
# Do:
gh issue list --json number --jq 'length'
# or
gh issue list --json number --jq 'map(.number) | add'

# Output: single number
```

---

## Anti-Patterns: What NOT to Do

| Anti-pattern | Cost | Fix |
|---|---|---|
| `gh pr list --json` (no fields) | Includes id, is_bot, nested author, timestamps, commits, files — 400+ chars/PR | Use `--json number,title,state` |
| `gh api repos/.../issues` (no jq) | Returns all fields of all issues across pagination — megabytes | Use `--jq '.[] \| {number, state}'` |
| `gh api --paginate` without limit | Fetches all matching items (1000+) | Use `--paginate \| jq '.[0:50]'` or GraphQL |
| `gh run view --log` unbounded | CI logs can be 100MB+ | Use `\| head -50` or `\| grep error` |
| `gh pr diff` unbounded | Large PRs can be 1MB+ | Use `\| head -200` or `--stat` |
| `gh api ... --pretty` in agentic context | Pretty-printing adds indentation (not human readable anyway) | Omit, use `--jq` instead |
| `GH_DEBUG=api` in agentic context | Adds HTTP request/response logging — doubles output size | **Never use in agents** |
| `GH_FORCE_TTY=1` in agentic context | Adds color codes and formatting — adds ~10% tokens | Never use; rely on NO_COLOR |

---

## Real-World Agentic Workflow

Example: "Which PRs are ready to merge?"

### Naive approach (inefficient)

```bash
gh pr list --json  # 400+ chars per PR
# ... agent reads megabytes, picks out mergeable ones
```

**Cost:** ~5000 tokens for 10 PRs

### Optimized approach

```bash
gh pr list --json mergeable,reviewDecision,number --jq \
  '.[] | select(.mergeable == "MERGEABLE" and .reviewDecision == "APPROVED") | .number'
```

**Cost:** ~500 tokens for 10 PRs (10x reduction!)

---

## Performance Checklist

When building agentic commands with gh:

- [ ] Set `NO_COLOR=1`, `GH_NO_UPDATE_NOTIFIER=1`, `GH_PAGER=cat`
- [ ] Use `--json` + `--jq` together (never raw JSON)
- [ ] Request only the fields you need (check minimal field table)
- [ ] Bound large outputs with `| head -n`, `| grep`, or `--jq 'first()'`
- [ ] Use GraphQL for bulk data fetches (100+ items)
- [ ] Reduce arrays to scalars when possible (`length`, `first`, `last`)
- [ ] Test output size: `<command> | wc -c` should be <1000 bytes per item for list commands
- [ ] Extract logins, not user objects (`--jq '.[].author.login'`)
- [ ] Avoid `GH_DEBUG`, `--pretty`, or other verbosity flags

---

## Further Reading

- See **SKILL.md** for the Agentic Output Principles overview
- See **output-formatting.md** for full `--json` fields reference
- See **search-and-query.md** for advanced REST/GraphQL patterns
- Official `jq` manual: https://jqlang.github.io/jq/manual/
