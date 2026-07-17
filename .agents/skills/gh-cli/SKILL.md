---
name: gh-cli
description: |
  Work with GitHub via the gh CLI: manage pull requests, issues, releases,
  repositories, Actions workflows, and make raw API calls. Use when the user
  needs to create PRs, review issues, trigger workflows, query the GitHub API,
  or do any GitHub operation from the command line.
license: MIT
compatibility: opencode
metadata:
  audience: developers
  tool: gh
  generated-by: cli-skill-generator
  generated-at: "2026-04-17"
  adopted-from: hand-authored
  adopted-at: "2026-04-17"
  tested-version: "2.90.0"
---

# GitHub CLI (gh) Skill

Master the GitHub CLI for pull requests, issues, releases, Actions, projects, and more.

## Agentic Output Principles

When using gh in an agentic context, **token efficiency is critical**. Always apply these rules — they override the examples in all other sections:

### Core Rules

1. **Never use raw JSON without `--jq`.** `--json` alone produces verbose nested objects with IDs, bot flags, and unused fields. Always chain: `--json <minimal-fields> --jq '<filter>'`.

2. **Request only the fields you need.** Check the Minimal Fields table below. Adding extra fields multiplies output size with no benefit.

3. **Bound large outputs.** Commands like `gh run view --log`, `gh pr diff`, `gh api --paginate` can emit megabytes. Always pipe through `| head -n 100`, `grep <pattern>`, or use `--jq 'first(.[])'`.

4. **Prefer text output for existence/status checks.** `gh pr list` (no flags) outputs compact tab-separated text (~80 chars/line). The same with `--json` (~300+ chars/line). Use text unless you need to branch on a specific field.

5. **Set agentic environment baseline** before running gh commands:
   ```bash
   export NO_COLOR=1               # no ANSI escape codes
   export GH_NO_UPDATE_NOTIFIER=1  # suppress update banners
   export GH_PAGER=cat             # disable interactive paging
   ```
   This eliminates color codes, update notifications, and paging prompts — all add noise tokens.

### Minimal Fields by Task

| Task | Command | Notes |
|---|---|---|
| Does PR exist? | `gh pr list --head <branch> --json number --jq 'length'` | Returns 0 or 1, tiny output |
| PR list summary | `gh pr list --json number,title,state` | Omit author, labels, timestamps unless needed |
| PR merge status | `gh pr view 42 --json mergeable,reviewDecision` | Only fields needed for merge decision |
| PR author | `gh pr list --json author --jq '.[].author.login'` | Extract just login, not id/is_bot/name |
| PR labels | `gh pr list --json labels --jq '.[].labels[].name'` | Project to label names only |
| Issue exists + state | `gh issue list --json number,state` | Two fields, minimal output |
| Issue assignees | `gh issue list --json assignees --jq '.[].assignees[].login'` | Project to logins only |
| Release latest | `gh release list --json tagName,isLatest --jq 'first'` | Single object, not array |
| CI run status | `gh run list --limit 1 --json status,conclusion` | Two fields, limit results |
| Workflow enabled? | `gh workflow list --json name,state` | Minimal info for decision |

---

<!-- generated -->

## Before You Start: Version Check

**This skill was generated for gh CLI v2.90.0 (released 2026-04-16).**

Always verify your installed version first:

```bash
gh --version
# Expected output: gh version 2.90.0 (2026-04-16)
```

**If your version differs,** inform the user:

> "Note: This skill was tested with gh v2.90.0. Your installed version is **X.Y.Z**.
> Some flags or subcommands may differ. To update:
> - **macOS/Linux (brew):** `brew upgrade gh`
> - **Ubuntu/Debian:** `sudo apt upgrade gh`
> - **Windows (winget):** `winget upgrade GitHub.cli`
> - Or download from: https://github.com/cli/cli/releases"

Most core commands remain stable across minor versions, so proceed with caution.

---

## Quick Start

Token-efficient patterns for common GitHub tasks:

```bash
# Authentication
gh auth login                    # Log in to GitHub
gh auth status                   # Check login status
gh auth token                    # Print current auth token

# Pull requests: check if exists
gh pr list --head $(git branch --show-current) --json number --jq 'length > 0'

# Pull requests: list open (text form is compact)
gh pr list --state open

# Pull requests: list with filtering (JSON + jq form)
gh pr list --json number,title,state --jq '.[] | "#\(.number) \(.title) [\(.state)]"'

# Pull request: check merge readiness
gh pr view 42 --json mergeable,reviewDecision,statusCheckRollup --jq \
  '"\(.mergeable) / reviews: \(.reviewDecision)"'

# Pull request: create
gh pr create --title "Fix bug" --body "Closes #123"

# Pull request: approve
gh pr review 42 --approve

# Issues: list open (text form)
gh issue list --state open

# Issues: list with custom filter
gh issue list --json number,state --jq '.[] | select(.state == "OPEN") | "#\(.number)"'

# Issues: create and link to PR
gh issue create --title "Bug found" --label bug

# Releases: create
gh release create v1.0.0 --notes "First release"

# Releases: check latest
gh release list --limit 1 --json tagName,isLatest --jq 'first.tagName'

# Repositories: clone
gh repo clone owner/repo

# GitHub Actions: check latest run status
gh run list --workflow ci.yml --limit 1 --json status,conclusion --jq \
  '.[0] | "\(.status) / \(.conclusion)"'

# GitHub Actions: view run (bounded output)
gh run view 12345 --log | head -100

# Projects: create and manage
gh project create --owner myorg --title "Q1 Roadmap"
gh project item-list 1 --owner myorg --jq '.[] | "\(.id) \(.title)"'

# API: list issues (with field projection)
gh api repos/{owner}/{repo}/issues --jq '.[].number'

# Search: find open PRs
gh search prs "state:open author:@me" --json number,title --jq '.[] | "#\(.number) \(.title)"'
```

---

## Core Commands

### Pull Requests (`gh pr`)

| Subcommand | Purpose |
|---|---|
| `create` | Create a new pull request |
| `list` | List pull requests in the repository |
| `status` | Show status of relevant pull requests |
| `checkout` | Check out a PR branch locally |
| `checks` | Show CI status for a single pull request |
| `close` | Close a pull request |
| `comment` | Add a comment to a pull request |
| `diff` | View changes in a pull request |
| `edit` | Edit a pull request |
| `lock` | Lock pull request conversation |
| `merge` | Merge a pull request |
| `ready` | Mark a draft PR as ready for review |
| `reopen` | Reopen a pull request |
| `revert` | Revert a pull request |
| `review` | Add a review (approve/request changes/comment) |
| `unlock` | Unlock pull request conversation |
| `update-branch` | Update PR branch from base |
| `view` | View details of a pull request |

**Key flags:**
- `--draft` — create as draft
- `--base <branch>` — target branch (default: repository default)
- `--head <branch>` — source branch
- `--reviewer <handle>` — request review
- `--label <name>` — add labels
- `--milestone <name>` — add to milestone
- `--assignee <login>` — assign user
- `--fill` — auto-populate from commits
- `--web` — open in browser
- `-R, --repo [HOST/]OWNER/REPO` — select another repository

### Issues (`gh issue`)

| Subcommand | Purpose |
|---|---|
| `create` | Create a new issue |
| `list` | List issues in a repository |
| `status` | Show status of relevant issues |
| `close` | Close issue |
| `comment` | Add a comment to an issue |
| `delete` | Delete issue |
| `develop` | Manage linked branches for an issue |
| `edit` | Edit issues |
| `lock` | Lock issue conversation |
| `pin` | Pin a issue |
| `reopen` | Reopen issue |
| `transfer` | Transfer issue to another repository |
| `unlock` | Unlock issue conversation |
| `unpin` | Unpin a issue |
| `view` | View an issue |

**Key flags:**
- `--label <name>` — add labels
- `--assignee <login>` — assign user
- `--milestone <name>` — add to milestone
- `--template <name>` — use issue template
- `--web` — open in browser
- `-R, --repo [HOST/]OWNER/REPO` — select another repository

### Repositories (`gh repo`)

| Subcommand | Purpose |
|---|---|
| `archive` | Archive a repository |
| `autolink` | Manage autolink references |
| `clone` | Clone a repository locally |
| `create` | Create a new repository |
| `delete` | Delete a repository |
| `deploy-key` | Manage deploy keys in a repository |
| `edit` | Edit repository settings |
| `fork` | Create a fork |
| `gitignore` | List gitignore templates |
| `license` | Explore licenses |
| `list` | List repositories owned by user or organization |
| `rename` | Rename a repository |
| `set-default` | Set default repo for current dir |
| `sync` | Sync fork with upstream |
| `unarchive` | Unarchive a repository |
| `view` | View repository info |

**Key flags:**
- `--public` / `--private` — visibility
- `--source <repo>` — fork source
- `--description <text>` — repo description

### Releases (`gh release`)

| Subcommand | Purpose |
|---|---|
| `create` | Create a new release |
| `delete` | Delete a release |
| `delete-asset` | Delete an asset from a release |
| `download` | Download release assets |
| `edit` | Edit release |
| `list` | List releases |
| `upload` | Upload assets to release |
| `verify` | Verify release attestation |
| `verify-asset` | Verify asset attestation |
| `view` | View release details |

**Key flags:**
- `--notes <text>` — release notes
- `--draft` — create as draft
- `--prerelease` — mark as pre-release
- `--target <branch>` — target branch for tag
- `-R, --repo [HOST/]OWNER/REPO` — select another repository

### Projects (`gh project`)

| Subcommand | Purpose |
|---|---|
| `close` | Close a project |
| `copy` | Copy a project |
| `create` | Create a project |
| `delete` | Delete a project |
| `edit` | Edit a project |
| `field-create` | Create a field in a project |
| `field-delete` | Delete a field in a project |
| `field-list` | List the fields in a project |
| `item-add` | Add a pull request or an issue to a project |
| `item-archive` | Archive an item in a project |
| `item-create` | Create a draft issue item in a project |
| `item-delete` | Delete an item from a project by ID |
| `item-edit` | Edit an item in a project |
| `item-list` | List the items in a project |
| `link` | Link a project to a repository or a team |
| `list` | List the projects for an owner |
| `mark-template` | Mark a project as a template |
| `unlink` | Unlink a project from a repository or a team |
| `view` | View a project |

**Key scope requirement:** Minimum token scope is `project`.

### Search (`gh search`)

| Subcommand | Purpose |
|---|---|
| `code` | Search code |
| `commits` | Search commits |
| `issues` | Search for issues |
| `prs` | Search for pull requests |
| `repos` | Search for repositories |

Syntax: `gh search issues "is:open label:bug"` (GitHub search syntax)

### GitHub Actions (`gh run`, `gh workflow`, `gh cache`)

#### `gh run`
| Subcommand | Purpose |
|---|---|
| `cancel` | Cancel a run |
| `delete` | Delete a run |
| `download` | Download run artifacts |
| `list` | List recent workflow runs |
| `rerun` | Rerun a failed run |
| `view` | View run details |
| `watch` | Watch run until completion |

#### `gh workflow`
| Subcommand | Purpose |
|---|---|
| `disable` | Disable a workflow |
| `enable` | Enable a workflow |
| `list` | List workflows |
| `run` | Trigger a workflow (needs `workflow_dispatch`) |
| `view` | View workflow details |

#### `gh cache`
| Subcommand | Purpose |
|---|---|
| `delete` | Delete GitHub Actions caches |
| `list` | List GitHub Actions caches |

### Artifact Attestations (`gh attestation`)

| Subcommand | Purpose |
|---|---|
| `download` | Download an artifact's attestations for offline use |
| `trusted-root` | Output trusted_root.jsonl contents for offline verification |
| `verify` | Verify an artifact's integrity using attestations |

**Alias:** `gh at`

### Repository Rulesets (`gh ruleset`)

| Subcommand | Purpose |
|---|---|
| `check` | View rules that would apply to a given branch |
| `list` | List rulesets for a repository or organization |
| `view` | View information about a ruleset |

**Alias:** `gh rs`

### Other Commands

| Command | Purpose |
|---|---|
| `gh browse` | Open repositories, issues, pull requests, and more in the browser |
| `gh codespace` | Connect to and manage codespaces |
| `gh gist` | Manage gists (create, view, edit, delete, clone, list, rename) |
| `gh label` | Manage labels |
| `gh org` | Manage organizations |
| `gh ssh-key` | Manage SSH keys |
| `gh gpg-key` | Manage GPG keys |
| `gh copilot` | Run the GitHub Copilot CLI (preview) |
| `gh agent-task` | Work with agent tasks (preview) |
| `gh skill` | Install and manage agent skills (preview) |

### Secrets & Variables (`gh secret`, `gh variable`)

| Command | Purpose |
|---|---|
| `gh secret set <NAME>` | Create/update a secret |
| `gh secret list` | List secrets |
| `gh secret delete <NAME>` | Delete a secret |
| `gh variable set <NAME> --body <value>` | Create/update a variable |
| `gh variable list` | List variables |
| `gh variable delete <NAME>` | Delete a variable |

### Authentication (`gh auth`)

| Subcommand | Purpose |
|---|---|
| `login` | Authenticate with GitHub |
| `logout` | Sign out |
| `refresh` | Refresh stored credentials |
| `setup-git` | Configure git to use gh for auth |
| `status` | Show auth status |
| `switch` | Switch GitHub account |
| `token` | Print current auth token |

### Configuration (`gh config`)

| Subcommand | Purpose |
|---|---|
| `clear-cache` | Clear the CLI cache |
| `get` | Print value of a configuration key |
| `list` | Print all configuration keys and values |
| `set` | Update configuration with a value |

### Utilities

| Command | Purpose |
|---|---|
| `gh alias` | Create command shortcuts |
| `gh api` | Make an authenticated GitHub API request |
| `gh completion` | Generate shell completion scripts |
| `gh extension` | Manage gh extensions |
| `gh licenses` | View third-party license information |
| `gh preview` | Execute previews for gh features |
| `gh status` | Print information about relevant issues, PRs, and notifications |

---

## JSON Output & Formatting

### Basic JSON output

```bash
# Get JSON with specific fields
gh pr list --json number,title,author
gh issue view 42 --json title,state,assignees

# Filter with jq
gh pr list --json number,title,author --jq '.[].author.login'

# Format with Go templates
gh pr list --json number,title --template '{{range .}}#{{.number}} {{.title}}{{"\n"}}{{end}}'
```

### Available JSON fields

Each command supports different fields. View them without arguments:

```bash
gh pr list --json         # Lists all available fields for pr list
gh issue view --json      # Lists all available fields for issue view
gh release list --json    # Lists all available fields for releases
```

### jq filter examples

```bash
# Count open PRs
gh pr list --json number --jq 'length'

# Get PR titles that mention "bug"
gh pr list --json title --jq '.[] | select(.title | contains("bug"))'

# List PR authors
gh pr list --json author --jq '.[].author.login'
```

### Template functions

Advanced formatting with `--template`:

- `tablerow <fields>...` — align fields vertically as table
- `tablerender` — render previously added rows
- `timeago <time>` — convert timestamp to relative time
- `timefmt <format> <time>` — format timestamp with Go layout
- `color <style> <input>` — colorize output
- `autocolor <style> <input>` — color only in terminal
- `hyperlink <url> <text>` — create terminal hyperlink
- `join <sep> <list>` — join array with separator
- `pluck <field> <list>` — extract field from all items
- `truncate <length> <input>` — limit length

Sprig functions also supported:
- `contains`, `hasPrefix`, `hasSuffix`, `regexMatch`

---

## Environment Variables

### Agentic Baseline

When running gh from an agent, always set these to eliminate noise tokens:

```bash
export NO_COLOR=1               # Disable ANSI escape codes (removes color sequences)
export GH_NO_UPDATE_NOTIFIER=1  # Suppress "new release available" banners
export GH_PAGER=cat             # Disable interactive paging (prevents less prompts)
```

Or inline per-command:

```bash
NO_COLOR=1 GH_NO_UPDATE_NOTIFIER=1 GH_PAGER=cat gh pr list
```

### All Environment Variables

| Variable | Purpose | Agentic use |
|---|---|---|
| `GH_TOKEN` | Authentication token (preferred over `GITHUB_TOKEN`) | Set in environment |
| `GH_ENTERPRISE_TOKEN` | Authentication token for GitHub Enterprise Server | Use for Enterprise hosts |
| `GH_HOST` | GitHub hostname (default: `github.com`) | Use for GitHub Enterprise |
| `GH_REPO` | Default repository (`OWNER/REPO`) | Avoids `-R` flag repetition |
| `GH_EDITOR` | Editor for composing text | Typically not used in agents |
| `GH_BROWSER` | Browser for opening links | Typically not used in agents |
| `GH_DEBUG` | Enable verbose output (`api` for HTTP details) | **Avoid in agents** (adds noise) |
| `GH_PAGER` | Paging program (e.g., `less`) | **Always set to `cat`** |
| `GH_CONFIG_DIR` | Config directory (default: `~/.config/gh`) | Use if custom location needed |
| `NO_COLOR` | Disable ANSI colors | **Always set to 1** |
| `CLICOLOR` | Set to 0 to disable ANSI colors | **Do not use** (contradicts NO_COLOR) |
| `CLICOLOR_FORCE` | Force ANSI colors when piped | **Do not use** (contradicts NO_COLOR) |
| `GH_NO_UPDATE_NOTIFIER` | Suppress update notifications | **Always set to 1** |
| `GH_NO_EXTENSION_UPDATE_NOTIFIER` | Suppress extension update notifications | Set to 1 if using extensions |
| `GH_FORCE_TTY` | Force terminal output | **Do not use** (adds formatting noise) |
| `GH_COLOR_LABELS` | Display labels using RGB hex color codes | Do not use in agents |
| `GH_ACCESSIBLE_COLORS` | Use customizable 4-bit accessible colors | Do not use in agents |
| `GH_PROMPT_DISABLED` | Disable interactive prompting | Set to 1 for non-interactive use |
| `GLAMOUR_STYLE` | Style for rendering Markdown | Do not use in agents |
| `GH_MDWIDTH` | Default max width for markdown wrapping | Typically not needed |
| `GH_SPINNER_DISABLED` | Replace spinner with textual progress | Set to 1 for cleaner output |
| `GH_ACCESSIBLE_PROMPTER` | Enable accessible prompts (preview) | Depends on use case |
| `GH_PATH` | Path to gh executable | Use if auto-detection fails |

<!-- /generated -->

---

## Specific Tasks

Deep dives into common workflows:

* **Agentic Patterns** — [references/agentic-patterns.md](references/agentic-patterns.md) ⭐
  - Token efficiency strategies, output bounding, GraphQL bulk queries, jq reduction patterns
  - **Read this for optimized agentic command patterns**
  
* **Pull Request Workflows** — [references/pr-workflows.md](references/pr-workflows.md)
  - Create with templates, merge strategies, review workflows, draft PR best practices
  
* **Issue Management** — [references/issue-workflows.md](references/issue-workflows.md)
  - Create with templates, triage, bulk edits, linking branches
  
* **Repository & Release Management** — [references/repo-management.md](references/repo-management.md)
  - Clone, fork, sync, create releases, upload assets, manage settings
  
* **Search & API** — [references/search-and-query.md](references/search-and-query.md)
  - Advanced search syntax, REST API calls, GraphQL queries, pagination
  
* **GitHub Actions & Workflows** — [references/actions-and-ci.md](references/actions-and-ci.md)
  - Run workflows, trigger with `workflow_dispatch`, view logs, re-run failed jobs
  
* **Output Formatting Reference** — [references/output-formatting.md](references/output-formatting.md)
  - Complete `--json` fields per command, `--jq` patterns, template functions

---

## Tips & Gotchas

- **Branch vs. repo context:** Many commands infer repo from `.git/config`. Use `-R owner/repo` to override.
- **Pagination:** Use `--paginate` with `--json` and `--slurp` to fetch all results as a single array.
- **Draft PRs:** Use `--draft` on create, then `gh pr ready <number>` to un-draft.
- **Linking issues:** Include `Fixes #N` or `Closes #N` in PR body to auto-close issue on merge.
- **Private tokens:** Always use `GH_TOKEN` or store in `gh auth login`. Never commit tokens to git.
- **Enterprise GitHub:** Set `GH_HOST=github.company.com` for GitHub Enterprise.
- **Project scope:** Use `gh auth refresh -s project` to grant project scope if needed.
- **Preview features:** Commands marked `(preview)` may change; use with caution in production automation.
- **`gh api` and JSON arrays:** Never use `-F field='["a","b"]'` for array payloads — `-F` sends the value as a string literal, not parsed JSON. GitHub API returns HTTP 422 "not an array". Use `--input /tmp/payload.json` instead. See [references/search-and-query.md](references/search-and-query.md) → "Complex payloads".

---

## Learn More

- Official docs: https://cli.github.com/manual
- Release notes: https://github.com/cli/cli/releases
- API docs: https://docs.github.com/en/rest
- GraphQL explorer: https://docs.github.com/en/graphql
