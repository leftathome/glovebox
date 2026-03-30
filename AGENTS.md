# Agent Instructions

This project uses **bd** (beads) for issue tracking. Run `bd onboard` to get started.

## Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work atomically
bd close <id>         # Complete work
bd dolt push          # Push beads data to remote
```

## Non-Interactive Shell Commands

**ALWAYS use non-interactive flags** with file operations to avoid hanging on confirmation prompts.

Shell commands like `cp`, `mv`, and `rm` may be aliased to include `-i` (interactive) mode on some systems, causing the agent to hang indefinitely waiting for y/n input.

**Use these forms instead:**
```bash
# Force overwrite without prompting
cp -f source dest           # NOT: cp source dest
mv -f source dest           # NOT: mv source dest
rm -f file                  # NOT: rm file

# For recursive operations
rm -rf directory            # NOT: rm -r directory
cp -rf source dest          # NOT: cp -r source dest
```

**Other commands that may prompt:**
- `scp` - use `-o BatchMode=yes` for non-interactive
- `ssh` - use `-o BatchMode=yes` to fail instead of prompting
- `apt-get` - use `-y` flag
- `brew` - use `HOMEBREW_NO_AUTO_UPDATE=1` env var

<!-- BEGIN BEADS INTEGRATION profile:full hash:d4f96305 -->
## Issue Tracking with bd (beads)

**IMPORTANT**: This project uses **bd (beads)** for ALL issue tracking. Do NOT use markdown TODOs, task lists, or other tracking methods.

### Why bd?

- Dependency-aware: Track blockers and relationships between issues
- Git-friendly: Dolt-powered version control with native sync
- Agent-optimized: JSON output, ready work detection, discovered-from links
- Prevents duplicate tracking systems and confusion

### Quick Start

**Check for ready work:**

```bash
bd ready --json
```

**Create new issues:**

```bash
bd create "Issue title" --description="Detailed context" -t bug|feature|task -p 0-4 --json
bd create "Issue title" --description="What this issue is about" -p 1 --deps discovered-from:bd-123 --json
```

**Claim and update:**

```bash
bd update <id> --claim --json
bd update bd-42 --priority 1 --json
```

**Complete work:**

```bash
bd close bd-42 --reason "Completed" --json
```

### Issue Types

- `bug` - Something broken
- `feature` - New functionality
- `task` - Work item (tests, docs, refactoring)
- `epic` - Large feature with subtasks
- `chore` - Maintenance (dependencies, tooling)

### Priorities

- `0` - Critical (security, data loss, broken builds)
- `1` - High (major features, important bugs)
- `2` - Medium (default, nice-to-have)
- `3` - Low (polish, optimization)
- `4` - Backlog (future ideas)

### Workflow for AI Agents

1. **Check ready work**: `bd ready` shows unblocked issues
2. **Claim your task atomically**: `bd update <id> --claim`
3. **Work on it**: Implement, test, document
4. **Discover new work?** Create linked issue:
   - `bd create "Found bug" --description="Details about what was found" -p 1 --deps discovered-from:<parent-id>`
5. **Complete**: `bd close <id> --reason "Done"`

### Auto-Sync

bd automatically syncs via Dolt:

- Each write auto-commits to Dolt history
- Use `bd dolt push`/`bd dolt pull` for remote sync
- No manual export/import needed!

### Important Rules

- ✅ Use bd for ALL task tracking
- ✅ Always use `--json` flag for programmatic use
- ✅ Link discovered work with `discovered-from` dependencies
- ✅ Check `bd ready` before asking "what should I work on?"
- ❌ Do NOT create markdown TODO lists
- ❌ Do NOT use external issue trackers
- ❌ Do NOT duplicate tracking systems

For more details, see README.md and docs/QUICKSTART.md.

## Landing the Plane (Session Completion)

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds

<!-- END BEADS INTEGRATION -->

---

## Connector Development

This section provides instructions for AI agents building glovebox connectors.

### Project Structure

```
connector/                  # Public library (import as github.com/leftathome/glovebox/connector)
  connector.go              # Interfaces: Connector, Watcher, Listener; Options, SetupFunc
  runner.go                 # Run() lifecycle, poll loops, health endpoints, signal handling
  staging.go                # StagingWriter, StagingItem, ItemOptions, atomic handoff
  checkpoint.go             # Checkpoint interface, file-backed implementation
  route.go                  # Router, Route
  metrics.go                # Metrics (OTel + Prometheus)
  content/                  # Optional content helpers
    mime.go                 # DecodeMIME -- parse MIME multipart messages
    html.go                 # HTMLToText -- strip HTML to plain text
    linkpolicy.go           # LinkPolicy -- URL fetch safety checks

connectors/                 # First-party connector implementations (one dir per connector)
  rss/                      # CANONICAL EXAMPLE -- reference this for all patterns
  imap/                     # Poll + Watch example

generator/                  # Scaffold generator
  main.go                   # CLI: go run ./generator new-connector <name>
  generate.go               # Template execution logic
  templates/                # Go templates for scaffold output

docs/
  connector-guide.md        # Human-readable connector development guide
  specs/                    # Design specifications
```

### How to Create a New Connector

Follow these steps in order:

**Step 1: Scaffold.**

```bash
go run ./generator new-connector <name>
```

Creates `connectors/<name>/` with: connector.go, config.go, main.go,
config.json, Dockerfile, README.md.

**Step 2: Define config.** Edit `connectors/<name>/config.go`. Keep
`connector.BaseConfig` embedded (provides `routes` field). Add
connector-specific fields with `json` struct tags.

**Step 3: Implement Poll.** Edit `connectors/<name>/connector.go`. The struct
must have fields: `config Config`, `writer *connector.StagingWriter`,
`router *connector.Router`. Implement
`Poll(ctx context.Context, cp connector.Checkpoint) error`. Inside Poll,
follow this sequence for each item:

1. Check `ctx.Err()` for cancellation
2. Call `c.router.Match(key)` to get destination (skip if no match)
3. Call `c.writer.NewItem(connector.ItemOptions{...})` to create staging item
4. Call `item.WriteContent(data)` to write content
5. Call `item.Commit()` to atomically stage the item
6. Call `cp.Save(key, value)` to advance checkpoint AFTER commit succeeds

Return `connector.PermanentError(err)` for non-retryable failures (bad
credentials, invalid config). Return plain errors for transient/retryable
failures (network timeouts, rate limits).

**Step 4: Optionally implement Watcher.** If the source supports persistent
connections or push notifications, add
`Watch(ctx context.Context, cp connector.Checkpoint) error` to your struct.
Watch should block until ctx is cancelled or an error occurs. The runner calls
Poll first to catch up, then Watch.

**Step 5: Optionally implement Listener.** If the source delivers webhooks,
add `Handler() http.Handler` to your struct. The runner serves the handler on
port (HealthPort + 1).

**Step 6: Wire main.go.** Edit `connectors/<name>/main.go`. Read config file
(env: `GLOVEBOX_CONNECTOR_CONFIG`, default: `/etc/connector/config.json`).
Unmarshal into your Config struct. Create connector instance. Call
`connector.Run(connector.Options{...})` with a `Setup` callback that sets
`writer` and `router`. Read credentials from environment variables, never from
config files.

**Step 7: Write tests.** Create `connectors/<name>/connector_test.go`. Use
`t.TempDir()` for staging and state directories. Use `httptest.NewServer` to
mock HTTP endpoints. Call `connector.NewStagingWriter`, `connector.NewRouter`,
and `connector.NewCheckpoint` directly. Call `Poll(context.Background(), cp)`
directly -- do NOT use `connector.Run` in tests. Verify: staged item count,
metadata.json fields, checkpoint values, deduplication on re-poll.

**Step 8: Verify.**

```bash
go test ./connectors/<name>/...
go vet ./connectors/<name>/...
```

**Step 9: Build.**

```bash
docker build -f connectors/<name>/Dockerfile -t glovebox-<name>:latest .
```

Build from repo root. The Dockerfile copies the full module context.

### Canonical Example

**Reference `connectors/rss/` for all patterns.** Specifically:

| File                  | What to learn                                               |
|-----------------------|-------------------------------------------------------------|
| `connector.go`        | Poll loop, per-item staging + checkpointing, error handling |
| `config.go`           | Config struct with BaseConfig embedding, XML types          |
| `main.go`             | Config loading, Setup callback, connector.Run invocation    |
| `connector_test.go`   | Test setup helper, mock HTTP, checkpoint dedup tests        |

### Key Patterns

**Staging an item:**

```go
dest, ok := c.router.Match("feed:" + feed.Name)
if !ok {
    return nil  // no route, skip
}

item, err := c.writer.NewItem(connector.ItemOptions{
    Source:           "<connector-name>",
    Sender:           senderName,
    Subject:          title,
    Timestamp:        ts,
    DestinationAgent: dest,
    ContentType:      "text/plain",
})
if err != nil {
    return fmt.Errorf("new staging item: %w", err)
}
if err := item.WriteContent([]byte(body)); err != nil {
    return fmt.Errorf("write content: %w", err)
}
if err := item.Commit(); err != nil {
    return fmt.Errorf("commit item: %w", err)
}
// Checkpoint AFTER commit
if err := cp.Save(cpKey, itemID); err != nil {
    return fmt.Errorf("save checkpoint: %w", err)
}
```

**Setup callback:**

```go
connector.Run(connector.Options{
    Name:       "<name>",
    StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
    StateDir:   os.Getenv("GLOVEBOX_STATE_DIR"),
    ConfigFile: configFile,
    Connector:  c,
    Setup: func(cc connector.ConnectorContext) error {
        c.writer = cc.Writer
        c.router = cc.Router
        return nil
    },
    PollInterval: 5 * time.Minute,
})
```

**Test setup:**

```go
func newTestConnector(t *testing.T) (*MyConnector, string, string) {
    t.Helper()
    stagingDir := t.TempDir()
    stateDir := t.TempDir()
    writer, err := connector.NewStagingWriter(stagingDir, "<name>")
    if err != nil {
        t.Fatalf("NewStagingWriter: %v", err)
    }
    router := connector.NewRouter([]connector.Route{
        {Match: "*", Destination: "test-agent"},
    })
    c := &MyConnector{writer: writer, router: router, config: Config{...}}
    return c, stagingDir, stateDir
}
```

### Common Mistakes to Avoid

1. **Advancing checkpoint before Commit.** Always call `cp.Save()` AFTER
   `item.Commit()` succeeds. If you save checkpoint first and Commit fails,
   the item is lost.

2. **Not checking ctx.Err() in loops.** Poll should check for context
   cancellation between items to support graceful shutdown.

3. **Returning bare errors for permanent failures.** Wrap with
   `connector.PermanentError()` if the error is not retryable (invalid
   credentials, malformed config). Default (unwrapped) errors are treated
   as transient and retried.

4. **Constructing metadata.json manually.** Use `ItemOptions` and `Commit()`.
   The framework handles metadata construction and validation.

5. **Empty DestinationAgent.** `Commit()` will fail if `DestinationAgent` is
   empty. Always route through the Router first.

6. **Skipping tests.** Every connector must have tests. Use mock HTTP servers
   and temp directories. Test: first poll, deduplication on re-poll, metadata
   fields, checkpoint persistence.

7. **Committing secrets.** Never commit credentials to git. Use environment
   variables injected by the deployment layer.

8. **Copying files into running containers.** Always rebuild the container
   image to deliver code changes.

9. **Using emoji in Go code or Python scripts.** Keep all output and string
   literals ASCII-compatible.

### Environment Variables

| Variable                    | Purpose                          |
|-----------------------------|----------------------------------|
| `GLOVEBOX_STAGING_DIR`      | Staging directory path           |
| `GLOVEBOX_STATE_DIR`        | Checkpoint state directory       |
| `GLOVEBOX_CONNECTOR_CONFIG` | Config file path                 |

Connector-specific credentials use their own env vars (e.g., `IMAP_HOST`,
`IMAP_PASSWORD`). Document these in the connector's README.

### Testing Requirements

- All connectors MUST have a `connector_test.go` file
- Tests MUST use `t.TempDir()` for staging and state directories
- Tests MUST mock external services (use `httptest.NewServer` for HTTP)
- Tests MUST verify: item staging, metadata correctness, checkpoint behavior
- Run `go test` and `go vet` before considering work complete
- If the connector runs in a container, test it in a container
