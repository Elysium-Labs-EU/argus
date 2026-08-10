# 1. Record architecture decisions

Date: 2026-08-10

## Status

Accepted

## Context

* argus carries design decisions that outlive any single issue or chat thread: the permission and deny floor model, gate and ship rules, worker lifecycle, config surface.
* Those decisions live scattered across commit messages and memory today, none of which survives a forge migration, an issue renumbering, or a new contributor arriving without the backstory.

## Decision

* Record every significant decision as an Architecture Decision Record under `docs/adr/`, one file per decision, numbered sequentially: `NNNN-short-title.md`.
* List every ADR in `docs/adr/index.md` by number, title, and status.
* Find the right ADR with `make adr-find Q="concept"`.
* Treat an accepted ADR as immutable. A changed decision gets a new ADR that supersedes the old one; the old one's status updates to say so.
* Never cite an issue or ticket number in an ADR body. Issues close, renumber, and migrate between forges; the ADR outlives all of that.

## Format

* Title: short imperative phrase.
* Date and Status: Proposed, Accepted, Superseded, or Deprecated.
* Context: the forces at play.
* Decision: what we agreed to.
* Consequences: what gets easier or harder, tradeoffs included.

## Consequences

* Significant decisions get a durable record that survives renumbering and migration.
* Contributors get one place to check before changing established design, instead of reconstructing intent from commit archaeology.
* An accepted ADR needs upkeep: supersede it explicitly instead of editing history in place.
