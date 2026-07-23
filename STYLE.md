# Style

Small values, small interfaces, errors wrapped with context. Nothing fancy.

## Rules

**Value semantics for config/data.**
Pointer only when it's stated why — avoiding a copy of a heavy struct, or
signaling intentional mutation. Never for "might be empty."
```go
func Write(path string, s *Status) error  // pointer: avoids copying a heavy
                                           // struct; Write does not mutate it
```

**Narrow inputs — pass only what the function needs.**
```go
func NewCLIReviewer(model string) CLIReviewer  // not the whole Config
```

**Small interfaces, defined at consumption point.**
1–3 methods. Define where used, not where implemented — a shared abstraction
across multiple concrete backends (e.g. `Forge`, spanning GitHub/GitLab/Gitea)
is the one legitimate exception.

**Errors as values, wrapped with context.**
```go
return nil, fmt.Errorf("querying systemd for daemon pid: %w", err)
```
Only a true leaf error (nothing underneath to wrap) skips `%w`.

**One derivation site per ambient fact.**
`os.Getenv`, `time.Now()`: read once, pass the resolved value down. Inject
`time.Now`-shaped fields on config structs for testability rather than calling
it inline.
```go
type Config struct {
    Now func() time.Time  // injected; tests supply a fixed clock
}
```
Two independently-correct-looking call sites for the same fact can still
silently diverge from each other.

**Subprocess calls: `exec.CommandContext`, never `exec.Command`.**
Everything shelled out to here is short-lived (`git`, `herdr`, `gh`, `claude
-p`) — no daemon lifecycle to manage, just context propagation so a cancelled
run actually stops the child process.

## Comments

Say why, not what — if the *why* isn't non-obvious, don't write a comment at
all. The code already says what it does.

**Never reference an issue or PR number in a comment.** Issues get closed,
renumbered, migrated between forges; a comment that only makes sense next to
`#15` rots the moment anyone reads it without that context loaded. Explain the
actual constraint or hazard in the comment itself.

```go
// Wrong: forces the reader to go look up #15 to understand the constraint
// This is the root-cause fix for issue #15.
func ensureFreshPane(...) error { ... }

// Right: the constraint stands on its own
// PaneRun delivers its command line as a literal chat message if the target
// pane already has a live agent session — refuse rather than attach to one.
func ensureFreshPane(...) error { ... }
```

The issue/PR number belongs in the commit message and PR description, not the
source — that's where the history lives, and it's expected to be read with
the forge's context available.

## Avoid

| Don't | Why |
|-------|-----|
| Package-level vars read implicitly | hidden dependency |
| Interfaces >3 methods (without a multi-backend reason) | hard to compose |
| Ambient lookup re-derived at multiple call sites | each copy can silently diverge |
| `exec.Command` (no context) | can't be cancelled |
| Comments describing *what* obvious code does | noise, rots as code changes |
| Issue/PR numbers inside comments | rots the moment the number is stale |
