# Merge to Main

Review the current code changes and create a comprehensive commit message, then merge to main.

## Workflow

### Step 1: Analyze Current State

First, determine the current git state:
1. Check if on a feature branch or main
2. Identify all changes:
   - `git status` - Check for staged/unstaged changes
   - `git log main..HEAD --oneline` - If on a branch, see commits ahead of main
   - `git diff main...HEAD --stat` - Summary of all changes vs main

### Step 2: Review the Code Changes

Thoroughly review all changes to understand what was done:
1. Read through the diff: `git diff main...HEAD` (or `git diff` if uncommitted changes exist)
2. Look at changed files and understand the context
3. Identify:
   - New features added
   - Bugs fixed
   - Refactoring done
   - Tests added/modified
   - Configuration changes

### Step 3: Generate Comprehensive Commit Message

Create a detailed commit message following this format:

```
<type>(<scope>): <short summary>

<detailed description of what changed and why>

Changes:
- <specific change 1>
- <specific change 2>
- ...

<optional: Breaking changes, migration notes, etc.>
```

**Types**: feat, fix, refactor, test, docs, chore, perf, style
**Scope**: The area of the codebase (e.g., user-profile, auth, api)

The summary should be:
- Under 72 characters
- In imperative mood ("add" not "added")
- Descriptive of the overall change

The description should:
- Explain the "why" behind the changes
- Reference any related issues (bd issue IDs if applicable)
- List all significant changes made

### Step 4: Present to User

Show the user:
1. Summary of all changes (files changed, insertions, deletions)
2. The proposed commit message
3. Ask for confirmation or modifications

### Step 5: Hermetic CI Gate (MANDATORY — merge is blocked until this passes)

Before any merge to main, the hermetic gate MUST run green. This is the real merge gate for the Coves backend — it builds the AppView from the working tree, stands up the isolated Docker stack (PDS, PLC, Jetstream, relay, Postgres), and runs every tier through `cmd/ci-report`, whose skip-inversion means an unallowlisted skip is a failure.

1. Run `make ci` from the repo root. It takes several minutes (image build + staged stack + full suite); do not background it and move on — wait for the verdict.
2. **Green is the only pass.** `make ci`'s own exit code is not sufficient on its own — confirm the `✓ CI GATE PASSED` line AND that `.ci-out/summary.json` reports `"ok": true` with `failed: 0` and no unexpected skips. `make ci` deliberately discards `go test`'s raw exit code; `cmd/ci-report` is the verdict.
3. **If it is red, STOP.** Do not merge, do not "fix it on main." Report the failing tests / unexpected skips / build failures to the user and resolve them on the branch first. A red gate blocking a merge is the gate working.
4. If Docker is not running or the stack cannot come up, that is a blocked gate, not a pass — surface it and stop; never merge on an unrun gate.
5. Only after a recorded green run, record the numbers (test count, wall-clock) and proceed.

Note: `make ci` requires this repo's hermetic stack and applies to the Coves backend only. It is intentionally absent from the global `merge-to-main` skill and from the mobile/frontend project commands, which have their own toolchains.

### Step 6: Execute Merge

Based on the current state, offer appropriate options:

**If on a feature branch with commits:**
1. Option A: Squash merge to main (recommended for cleaner history)
   ```
   git checkout main
   git merge --squash <branch>
   git commit -m "<comprehensive message>"
   ```
2. Option B: Regular merge to main
   ```
   git checkout main
   git merge <branch>
   ```

**If on a feature branch with uncommitted changes:**
1. First commit the changes with the comprehensive message
2. Then offer merge options as above

**If on main with uncommitted changes:**
1. Commit directly with the comprehensive message

### Step 7: Cleanup (Optional)

After successful merge, offer to:
- Delete the feature branch locally: `git branch -d <branch>`
- Delete the feature branch remotely: `git push origin --delete <branch>`

## Important Notes

- The hermetic CI gate (Step 5) is mandatory and blocking — never merge on a red or unrun `make ci`
- Always show the user what will happen before executing
- Never force push or use destructive operations without explicit confirmation
- If there are merge conflicts, stop and help the user resolve them
- Preserve the Co-Authored-By trailer as required by the repo guidelines
