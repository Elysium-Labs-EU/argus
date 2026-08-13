# CLAUDE.md

## Architecture Decisions

Before changing established design (the permission and deny floor model,
gate and ship rules, worker lifecycle, config surface), check `docs/adr/`
first. Run `make adr-find Q="concept"` to locate the relevant record; it
prints each match's status alongside its path.

## Orchestrator hooks

argus exports `ARGUS_WORKER=1` into every worker's spawn environment. An
orchestrator-global Claude Code hook that must not run inside an argus worker
should guard on that signal at the top:

```sh
[ -n "$ARGUS_WORKER" ] && exit 0
```

Workers may use both the todo tool and typed tasks — argus's plan-evidence
gate accepts either as proof of a real plan — so this guard is the only
boundary between an orchestrator-intended hook and a worker's own turn.
