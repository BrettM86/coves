# Fix PR Comments

Analyze the current changed work and fix PR review comments using parallel subagents to avoid context rot.

## Input

The user will paste PR review comments after the command. The comments are provided below:

$ARGUMENTS

## Workflow

### Step 1: Analyze Current State

Run these commands to understand the full scope of changes:
1. `git diff --stat` — overview of changed files
2. `git diff` — full diff of all current changes (staged + unstaged)
3. `git status` — current working tree state

Read through the diff carefully. Build a mental model of what was changed, which files are involved, and the architecture of the changes.

### Step 2: Parse and Group the PR Comments

From the pasted PR comments above, identify every distinct issue. Group them into **2-3 independent chunks** based on:
- Which files they touch (keep file-adjacent issues together)
- Logical coupling (issues that affect each other should be in the same chunk)
- Roughly equal workload per chunk

**Grouping heuristic:**
- **≤3 issues total** → 2 chunks
- **4+ issues total** → 3 chunks
- Never put tightly coupled issues in different chunks (e.g., if fixing issue A changes code that issue B also references, they go together)

### Step 3: Launch Subagents in Parallel

For each chunk, launch a `general-purpose` subagent via the Task tool. All subagents should be launched in a **single message** so they run concurrently (foreground, not background).

Each subagent prompt MUST include:
1. **The full git diff** (or the relevant portions for their files) so they understand the current state
2. **The specific PR comments** they are responsible for fixing, quoted verbatim
3. **Clear instructions**: fix the issues described, follow CLAUDE.md guidelines, and run `go vet ./...` after making changes to verify correctness
4. **File scope**: explicitly list which files they should be reading/editing

**Important subagent instructions to include:**
- "You are fixing PR review comments on existing changed code. Read the relevant files first, then make targeted fixes."
- "After making changes, run `go vet ./...` and `go build ./...` to verify no issues were introduced."
- "Follow all CLAUDE.md guidelines — parameterized queries, proper error handling, context.Context everywhere, no stubs."
- "Do NOT refactor beyond what the PR comment asks for. Make minimal, focused fixes."

### Step 3: Review Results

After all subagents complete:
1. Run `go vet ./...` and `go build ./...` to verify the full project compiles cleanly
2. Run `git diff --stat` to summarize what was changed
3. Report to the user:
   - Which PR comments were addressed
   - What changes were made
   - Whether `go vet` and `go build` pass
   - Any comments that couldn't be fully addressed and why

## Notes

- The goal is **isolated, focused fixes** — each subagent works on its own slice without polluting context for other fixes.
- If two subagents need to edit the same file, group those issues together in one chunk to avoid conflicts.
- Prefer fewer, larger chunks over many tiny ones — the overhead of each subagent matters.
- If a PR comment is unclear or seems wrong, flag it in the results rather than guessing.
