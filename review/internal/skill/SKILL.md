---
name: review
description: Interact with the local code review tool to inspect review comments, reply to feedback, edit comments, resolve threads, and snapshot revisions.
---

# Review Skill

The `review` tool manages local code review sessions across Jujutsu (`jj`) and Git repositories.
When addressing human review feedback, use this skill to inspect comments, make requested changes, reply, and record clean snapshots.

## Addressing Review Comments Workflow

When you are asked to address comments or review feedback:

1. **Check Open Comments**:
   ```bash
   review comments
   ```
   Or for structured machine-readable JSON output:
   ```bash
   review comments -json
   ```
   When no change is specified, `review comments` iterates across all non-working pending changes ordered oldest-to-newest (parent commits before children).
   Unpublished draft comments are excluded by default to respect reviewer privacy; use `-drafts` if explicitly needed.

2. **JSON Output Schema**:
   Each JSON thread object contains:
   ```json
   {
     "change_key": "string (jj change id or git commit/change-id)",
     "commit": "string (full commit hash)",
     "subject": "string (commit description subject)",
     "thread_id": 1,
     "file": "path/to/file.go",
     "side": "R",
     "line": 42,
     "end_line": 45,
     "current_line": 43,
     "current_end_line": 46,
     "stale": false,
     "resolved": false,
     "context": "code snippet with line numbers",
     "messages": [
       {
         "id": 1,
         "thread_id": 1,
         "author": "reviewer",
         "body": "Comment text",
         "draft": false,
         "created_at": "2026-11-05T12:00:00Z"
       }
     ]
   }
   ```

3. **Fix Changes (Oldest to Newest)**:
   In Jujutsu (`jj`), always address comments on older parent commits first before downstream children:
   ```bash
   # Stack a temporary commit on the change being fixed
   jj new <change>
   # Make requested edits and verify tests pass
   # Squash fixes cleanly into the commit
   jj squash
   ```

4. **Reply and Resolve Threads**:
   Once the fix is applied and verified, reply to the thread and mark it resolved in one step:
   ```bash
   review reply -from <your-name> -resolve <thread-id> "Fixed in latest commit. Verified tests pass."
   ```

5. **Edit Comments if Needed**:
   To update or correct an existing comment:
   ```bash
   review edit <message-id> "Updated clarification..."
   ```

6. **Record a Snapshot**:
   Once all changes for the revision are complete and tests pass, take a snapshot:
   ```bash
   review snapshot
   ```
   When run without arguments, `review snapshot` snapshots all pending non-empty commits and pins them in VCS. Reviewers can then compare snapshot-to-snapshot diffs.

## CLI Command Reference

- `review [-a addr] [-n] [-db file]`: Launch the review web server and open the browser (directs to the current repo view).
- `review stop [-db file]`: Stop the running review server.
- `review restart [-a addr] [-n] [-db file]`: Restart the review server.
- `review comments [-json] [-all] [-drafts] [-s snap] [-c lines] [change]`: View review threads with surrounding code context (filter by snapshot number with `-s`).
- `review add [-from author] [-side R|L] [-s snap] [-draft] <change> <file[:line[-end]]> [text]`: Add a comment thread (published immediately by default; `/COMMIT_MSG` can be targeted as `<file>`).
- `review reply [-from author] [-draft] [-resolve] <thread-id> [text]`: Reply to a comment thread (published immediately by default).
- `review edit <message-id> [text]`: Edit a comment or reply message body.
- `review resolve <thread-id>...`: Mark threads as resolved.
- `review unresolve <thread-id>...`: Reopen resolved threads.
- `review publish [change]`: Publish draft comments for a change (or all drafts in the repo if omitted).
- `review snapshot [-nopin] [change...]`: Record a snapshot (all pending changes if omitted).
- `review snapshots [change]`: List recorded snapshots and parent revisions for a change.
- `review search [-i] <query> [change]`: Search inside changed files and commit messages.
- `review forget [repo...]`: Stop tracking a repository in the review database.
- `review gc [-dry-run] [-keep-missing]`: Clean up missing repositories, stale metadata, and unreferenced pins.
- `review skill [-print] [-install] [-project] [-o dir]`: Print or install agent skill documentation.
