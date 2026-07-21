# Worker runtime adapter protocol

argus spawns each worker as a shell command typed into a herdr pane. By
default that command runs directly on the host, in the same OS user and
filesystem as argus itself — credproxy (`internal/credproxy`) keeps the real
`ANTHROPIC_API_KEY` out of the worker's environment, but the worker still has
unrestricted read access to `~/.ssh`, `~/.claude`, `~/.aws`, and unrestricted
outbound network. A runtime adapter closes that gap by running the worker in
an isolated environment (a container, a namespace, or nothing at all) whose
mount table and network simply don't contain those paths and hosts.

Read this document and you don't need to read argus core to write one.

## The contract

An adapter is an executable named `argus-runtime-<name>` resolved via
`exec.LookPath` on `$PATH` — the same convention `git` uses to find
`git-<subcommand>`, and the same one eos uses for its sink plugins
(`eos-plugins/PROTOCOL.md`). `--worker-runtime <name>` (or `worker_runtime:`
in argus's config file) selects `<name>`; the default is `none`, so existing
installs are unaffected until an operator opts in.

argus core never hardcodes `docker`, `podman`, or any other backend's flags.
It knows only the adapter's name and the three environment variables below.

### Invocation

argus execs the adapter with no arguments and three environment variables:

| Variable                | Contents                                                          |
|--------------------------|--------------------------------------------------------------------|
| `ARGUS_RUNTIME_WORKTREE` | Absolute path to the worker's git worktree.                       |
| `ARGUS_RUNTIME_ENV`      | JSON object (`{"KEY":"value",...}`) of env vars the worker process needs — today, the credproxy sentinel and its base URL. |
| `ARGUS_RUNTIME_CMD`      | The inner command to run, e.g. `claude --permission-mode auto "<initial prompt>"`. |

### Output

The adapter prints **exactly one line to stdout**: the final shell command
line argus types into the herdr pane. That's it — argus does not pipe stdin
or stdout to the adapter beyond reading that one line, and the adapter's own
process exits immediately after printing it (the printed line, not the
adapter, is what actually runs the worker — herdr executes it in the pane).

- Non-zero exit code, or empty stdout, is a **hard error**: argus aborts the
  spawn and reports the adapter by name, mirroring eos's missing-plugin error
  UX. There is no silent fallback to running the worker unwrapped — that
  would defeat the point of configuring a runtime in the first place.
- A missing binary (not found on `$PATH`) is the same hard error, naming the
  adapter argus was told to use.

### `argus-runtime-none`

The trivial adapter and the default: echoes `ARGUS_RUNTIME_CMD` back
unchanged. This is today's plain `cd worktree && claude ...` behavior, with
no isolation added — exactly what an operator who configures no runtime (or
explicitly passes `--worker-runtime none`) already gets.

## Env handling is not uniform across backends — and that's fine

Today's `SpawnCommand` (`internal/supervisor/loop.go`) builds env handling as
shell prefixes on the *host* shell: `env -u CODEBERG_TOKEN -u GITHUB_TOKEN ...
KEY='value' ... launcher`. The `-u` list scrubs secrets the pane inherited
from the host's own environment (forge tokens argus itself uses, never the
worker); the `KEY=value` list injects the credproxy sentinel. Order matters:
scrub first, then inject, then launch.

**`argus-runtime-none` keeps this shape exactly as-is.** It is still the same
host shell, so the scrub is still load-bearing — there is nothing else
standing between the worker and the host's real environment.

**A container backend collapses this.** The container's environment starts
empty (or at whatever the image sets) — the host's forge tokens were never
in it to begin with, so there is nothing to `env -u` away. The adapter only
needs to turn `ARGUS_RUNTIME_ENV` into `-e KEY=value` flags on `docker
run`/`podman run`. No denylist is required for that backend, because the
allowlist (only what's in `ARGUS_RUNTIME_ENV`) is structural: nothing else
reaches the container's environment at all.

Adapters should document which shape they implement. `argus-runtime-none` is
the scrub-shape; `argus-runtime-docker` and `argus-runtime-podman` are the
allowlist-shape.

## The worktree/git mount problem is the adapter's problem

A git worktree's `.git` file is not a directory — it's a text file pointing
at the parent repository's real `.git` directory by absolute host path (e.g.
`gitdir: /Users/you/repo/.git/worktrees/feat-x`). Bind-mounting only the
worktree into a container leaves that path unresolvable inside the
container's mount namespace, so `git status`, `git diff`, and `git push`
inside the container will fail.

This is **not** something argus core solves — it doesn't know or care what
mount mechanism a given backend uses. Each adapter that isolates the
filesystem is responsible for making the parent `.git` directory resolvable
at the same absolute path inside the isolated environment, by whatever means
fits that backend (e.g. also bind-mount the parent `.git` dir at its host
path, or rewrite `GIT_DIR`/`GIT_COMMON_DIR` via a var it adds to
`ARGUS_RUNTIME_ENV` before templating the inner command).

That mount must be **read-write**, not read-only. This was tried read-only
first and empirically failed: a worktree's own operational state (its index,
`HEAD`, and per-worktree refs/locks) lives inside
`<repo>/.git/worktrees/<name>/`, i.e. inside the *parent* `.git` dir, not
inside the worktree directory itself — `git commit` writes an index lock
there and fails with "Read-only file system" if it can't. Confirmed against
real Docker, not assumed. Read-write here does not widen the worker's reach
beyond what it already has outside a container (its own repo's `.git` dir);
it does not expose any other repo or host path.

Confirm `git status`/`git log`/`git commit`/`git push` all work from inside
the isolated environment before shipping a new adapter — read-only operations
succeeding is not sufficient evidence that the mount is right.

## Credential and network scoping are the adapter's job

- **credproxy reachability.** `ARGUS_RUNTIME_ENV` carries the proxy's
  `ANTHROPIC_BASE_URL` as a `http://127.0.0.1:<port>` loopback address. That
  address is not reachable by name from inside a container's own network
  namespace — the adapter is responsible for making it reachable however its
  backend requires (Docker Desktop/OrbStack: `host.docker.internal`; Podman:
  `host.containers.internal`), typically by rewriting the host portion of the
  URL before it reaches the inner command.
- **Egress allowlisting.** Restricting the worker's outbound network to only
  credproxy and the git remote (denying everything else) is backend-specific
  setup — a dedicated container network with no default route out, mirroring
  the `vp-internal` network + egress-proxy pattern documented in
  `defending-code-reference-harness/docs/agent-sandbox.md`. If cleanly
  scoping git-remote egress turns out awkward for a given backend, the
  fallback is to strip `git push` out of the container entirely and let
  argus (on the host) push after gating the diff — that's a bigger
  control-flow change, only worth it if network scoping doesn't work out.

## What argus core does *not* do

- It does not know `docker` or `podman` flag syntax, image names, or network
  names — that all lives in the adapter script.
- It does not pipe the worker's stdin/stdout through the adapter process; the
  adapter's job ends the moment it prints the one command line. herdr, not
  the adapter, is the process supervision layer for the worker itself.
- It does not fall back silently when an adapter is missing or fails — a
  configured runtime that can't run is an error, not a downgrade to
  unwrapped execution.

## Reference adapters

- `runtimes/argus-runtime-none` — the trivial pass-through adapter (also the
  built-in default behavior when no `--worker-runtime` is given).
- `runtimes/argus-runtime-docker` — wraps the inner command in `docker run
  --rm -v '<worktree>:/work' -w /work --network argus-worker-net -e ...
  <image> <cmd>`, targets any Docker-compatible engine (OrbStack, Docker
  Desktop), and rewrites the credproxy base URL to `host.docker.internal`.
  Smoke-tested against real Docker: the filesystem isolation claim holds
  (`~/.ssh`, `~/.claude`, `~/.aws` do not exist inside the container) — but a
  plain `docker network create argus-worker-net` does not by itself restrict
  egress, so outbound traffic to arbitrary hosts still succeeds today. The
  network-egress-allowlist half of "Credential and network scoping" above is
  not yet built into this script; making `argus-worker-net` internal/
  restricted (or fronting it with an egress proxy) is follow-on work for
  whoever deploys this adapter for real.

A `podman` adapter is the same shape as the docker one (`podman run`,
`host.containers.internal`) and can be added by anyone without touching
argus core, since the contract above is all a new adapter needs to satisfy.
An Apple `container`-based adapter is a placeholder for later — same
contract, added once the underlying tool is officially supported on the
target macOS version.
