# Development Principles

## Notebook Principles
- Notes in `notes/` directory, organized by topic subdirectories
- Use markdown with clear headings
- Keep notes atomic: one concept per file when possible

## Core Principles
- Pragmatic over dogmatic
- Single responsibility per function/class
- Minimal comment style
- After modification/refactoring/generation codes: no code summaries, only minimal change overviews
- No summary.md or similar files in interactive mode
- No "Project Structure" nor "Troubleshooting" in docs or skills as it's not persistent
- Designing and Discussing before Implementing large tasks
- show design without detail codes
- use utf8 by default, especially for Chinese text
- Check before using the Write tool: split into chunks if content exceeds 50 lines or 2000 characters or 3000 tokens

## Code Restrictions
- Comments only in English
- Single function < 200 lines
- Loop nesting <= 3 levels
- Sh scripts can be invokable via absolute paths: No dependences on working dir.

## TEST
- Ask before running tests

## COMMIT MESSAGE
- No info beyond changes (e.g., author)

## MCP
- serena
    * Priority use, unless alternative methods can significantly reduce token consumption

## use Less tokens

- use mv command instead of creating then removing old, unless they can significantly reduce token consumption

## troubles

- try to split large write content into splits when caught error with "Error: InputValidationError: Write failed due to the following issues: `file_path` is missing or `content` is missing"
