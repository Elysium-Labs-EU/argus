# CLAUDE.md

## Architecture Decisions

Before changing established design (the permission and deny floor model,
gate and ship rules, worker lifecycle, config surface), check `docs/adr/`
first. Run `make adr-find Q="concept"` to locate the relevant record; it
prints each match's status alongside its path.

New ADRs are named with a minute-precision datetimestamp id,
`DDMMYYHHMM-slug.md` (day, month, two-digit year, hour, minute, 24h), e.g.
`docs/adr/1308261950-slug.md` — this self-allocates a unique id from creation
time so two workers authoring ADRs in parallel never collide on a number
(see `docs/adr/1308261950-adopt-datetimestamp-adr-ids.md`). Existing ADRs
`0001`-`0009` keep their sequential ids and are never renamed.

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
