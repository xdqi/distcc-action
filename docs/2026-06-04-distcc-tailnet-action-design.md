# distcc-action — Design

**Date:** 2026-06-04
**Status:** Design approved; ready for implementation planning.

## Summary

A reusable GitHub Action that turns several GitHub-hosted runners in a single
workflow into an ephemeral `distcc` compilation farm, connected by a temporary
Tailscale mesh. A coordinator runner fans its C/C++ compilation out to N worker
runners and tears the mesh down when the build finishes.

The Action is a single statically-linked Go binary (`CGO_ENABLED=0`) that
embeds a Tailscale node via the [`tsnet`](https://pkg.go.dev/tailscale.com/tsnet)
library. It does networking, orchestration, and teardown; the actual compilation
is delegated to stock `distcc`.

The whole thing is "for fun / architecture exploration" first and a publishable
Action second: nobody has shipped distcc + Tailscale + GitHub Actions glued
together as a reusable Action, and the lifecycle problem (sibling jobs that must
rendezvous and tear down with no native primitive) is the interesting part.

## Goals

- A single workflow spins up 1 coordinator job + N worker jobs (matrix), all
  GitHub-hosted runners, fully self-contained (no external KV, no long-lived
  infra).
- Workers form a distcc farm reachable by the coordinator over a temporary,
  ephemeral Tailscale mesh.
- The coordinator's normal build steps (e.g. `make -j`) transparently fan out to
  the workers via `distcc`.
- Clean teardown: when the coordinator's build finishes, workers exit promptly
  on their own; nobody hangs waiting.
- Publishable: `README` + version tag + example workflow + ACL/OAuth notes, so a
  third party can `uses:` it.

## Non-Goals (v1)

- **Cross-compilation / `zig cc` / custom wrapper compilers.** distcc requires
  byte-identical compilers on coordinator and workers; custom toolchains with
  bespoke sysroots cannot guarantee this. v1 is honestly scoped to
  **same-architecture native C/C++** (gcc/g++/clang) only.
- **A benchmark proving it is faster than a single runner.** Cross-tailnet
  transport overhead may make small builds no faster; v1 success is "it stands
  up, distributes correctly, and is reusable," not "it's faster."
- **Reimplementing distcc.** The Go binary does networking/orchestration only;
  compilation distribution stays with stock distcc.

## Background / Research

- No existing project combines distcc + Tailscale + GitHub Actions as a reusable
  Action. Closest prior art: `tailscale/github-action` (mesh only),
  `elijahr/build-farm` (distcc but workers are docker containers inside one
  runner, not cross-runner), `distcc/distcc` (the tool itself).
- The hard problem, confirmed by research: GitHub Actions has **no native
  primitive** for sibling jobs to discover each other or to keep one job alive
  while peers reach a rendezvous. This must be solved with an out-of-band
  mechanism — here, the Tailscale mesh itself plus per-run naming.

## Verified Facts (from the local docker spike)

These were established by a local multi-container spike, not assumed. They are
the load-bearing facts behind the design.

1. **distcc over a SOCKS5/proxy path works.** A plain TCP `distccd` reached
   through a SOCKS5 hop (via `socat` port-forward, distcc using a normal
   `host:port` entry) compiles correctly. distcc need not know a proxy exists.
   *(tsnet replaces the socat+SOCKS5 layer with direct `tsnet.Dial`; see "Open
   implementation question".)*

2. **A Tailscale OAuth client secret can be used directly as `--authkey`**, as
   long as `--advertise-tags` names a tag the client is authorized for. OAuth
   clients do not expire on the 90-day auth-key rotation schedule, which is why
   CI should use them. (The Go binary will mint/join equivalently via tsnet.)

3. **userspace Tailscale works in a container**, and isolating each container on
   its own docker network forces real DERP routing in both directions (avoids a
   same-bridge shortcut that otherwise masks true cross-host behavior). This made
   the spike a faithful model of cross-runner networking.

4. **`tailscale set --shields-up=true` does NOT block `tailscale ping`.**
   `tailscale ping` is control-plane and is immune to shields-up (verified: 5/5
   pongs via DERP after shields-up=true). An earlier design that signaled
   teardown via "shields-up → worker ping fails" is therefore **invalid** and was
   discarded.

5. **Working teardown mechanism (verified end-to-end):** the coordinator goes
   offline; each worker polls tailnet status for the coordinator peer's `Online`
   flag; after N
   consecutive "gone" observations (~5s at 1s interval) the worker logs out and
   exits. Both workers exited synchronously ~5s after the coordinator went down.
   The coordinator does not need to wait for workers after going down — everyone
   exits independently.

6. **Robustness rules learned the hard way:**
   - Per-run unique prefix (use `github.run_id`) for node hostnames AND any
     docker networks. Repeated runs otherwise leave same-name ephemeral zombie
     nodes; a worker resolving the hostname can lock onto a dead (`rx 0`) node and
     hang forever in its pre-warm loop.
   - **Never trust `tailscale ping`'s exit code** — it returns rc=0 even on
     timeout. Parse status / output, not rc. (In tsnet this becomes a typed Go
     call, removing the trap entirely.)

## Architecture

### Form

A single Go binary, `CGO_ENABLED=0` (fully static, no libc dependency, so the
same binary runs on any runner image and inside any container regardless of
glibc/musl). Packaged as a GitHub Action with a `mode` input: `coordinator` or
`worker`. The binary embeds a Tailscale node via `tsnet` — no system `tailscaled`,
no `tailscale` CLI, no userspace SOCKS5 proxy.

### Topology (single workflow, jobs start in parallel)

```
workflow
├── job: workers   strategy.matrix: [1 .. N]            (parallel)
│     uses: <repo>@v1  with: { mode: worker, ... }
│        - tsnet join  (hostname=<run_id>-worker-<i>, tag:ci-distcc)
│        - start distccd (plain TCP listener)
│        - expose distccd to the coordinator over the tailnet
│        - guard loop: watch for coordinator peer to go offline
│
└── job: build   (coordinator)                          (parallel)
      uses: <repo>@v1  with: { mode: coordinator, expected-workers: N, ... }
        main:
          - tsnet join (hostname=<run_id>-coordinator, tag:ci-distcc)
          - wait until N <run_id>-worker-* peers are Online
          - for each worker, tsnet-forward a local port -> worker:distccd
          - export DISTCC_HOSTS (localhost ports) + DISTCC_J to the job env
          - hand control back to the user's build steps
        (user build runs here: make -j$DISTCC_J  CC="distcc gcc")
        post (auto):
          - go offline (close the tsnet node so the coordinator peer drops)
          - exit (workers detect departure and exit on their own)
```

### What tsnet removes (vs. the CLI-based spike)

`tailscaled` daemon, `--tun=userspace-networking`, the SOCKS5 proxy port,
`socat` port bridges, `jq`, `tailscale status` text parsing, and the unreliable
CLI exit codes — all replaced by typed `tsnet` / `LocalClient` calls. The
coordinator dials workers with `tsnet.Dial`; status is read from a Go struct.

### Compilation path (unchanged: stock distcc)

The coordinator tsnet-forwards each worker's `distccd` onto a local port, then:

```
DISTCC_HOSTS="127.0.0.1:3701/4,lzo 127.0.0.1:3702/4,lzo ..."
DISTCC_J=<sum of worker slots>          # exported for the user's `make -j`
CC="distcc gcc"   (or "sccache distcc gcc" if sccache enabled — see below)
```

distcc treats the workers as a localhost cluster and is unaware of the tailnet.

### Lifecycle / teardown (verified mechanism, fact #5)

- Prefix every node hostname and resource with `github.run_id` (fact #6): kills
  zombie same-name nodes and isolates concurrent runs.
- Workers pre-warm: wait until the `<run_id>-coordinator` peer is present and
  Online before entering the guard loop (so a not-yet-online coordinator doesn't
  trigger immediate exit).
- Teardown signal = coordinator going offline. Worker confirms with N consecutive
  "coordinator peer not Online" observations (default 5, at 1s interval) to
  tolerate DERP jitter, then exits. ~5s teardown latency.
- The coordinator's `post` step triggers automatically after the user's build
  steps; the user writes no explicit teardown call.
- Optional refinement (tsnet makes this cheap): coordinator also exposes an
  explicit tsnet beacon (a TCP/HTTP endpoint) that it closes on teardown, giving
  workers a more immediate and unambiguous "done vs. crashed" signal than peer
  disappearance alone. Decided at implementation time; peer-offline is the
  guaranteed baseline.

### Compiler consistency (container optional)

distcc requires identical compilers on both sides. Two supported ways:

- **Container (user opt-in):** the user runs all jobs under the same
  `container:` image, so gcc is byte-identical. Strongest guarantee.
- **Bare runner (default):** all jobs run on the same `ubuntu-latest`; same-epoch
  runner images carry the same gcc. Simplest.

The Action does not force either — the static Go binary runs in both. Docs must
state the honest boundary: same-architecture native C/C++ only; no custom/cross
wrapper compilers.

### Rendezvous (tailnet self-discovery)

No external KV / gist / artifact. Discovery is via the tailnet itself: the
coordinator lists peers whose hostname starts with `<run_id>-worker-` and are
Online; workers watch for the `<run_id>-coordinator` peer. The `tsnet`
`LocalClient().Status()` provides this as typed data.

### Auth

Tailscale OAuth client (scope `auth_keys`, tag `tag:ci-distcc`) provided by the
user as GitHub secrets. OAuth avoids the 90-day auth-key rotation. The tag must
be declared in the user's ACL `tagOwners`.

## Action Interface

Single composite/binary Action with a `mode` input. Indicative `with:` inputs
(finalized during implementation):

- `mode`: `coordinator` | `worker` (required)
- `oauth-client-id`, `oauth-secret`: Tailscale OAuth credentials (required)
- `tags`: tailnet tags to advertise (default `tag:ci-distcc`)
- `expected-workers`: N (coordinator only)
- `run-prefix`: defaults to `github.run_id`
- `distcc-slots`: per-worker job slots (default 4)
- `ping-fail-threshold`, `poll-interval`: teardown tuning (defaults 5 / 1s)
- `sccache`: optional, default off (see below)

The user supplies the matrix and the build command in their own workflow; the
Action provides only networking + orchestration + teardown and exports
`DISTCC_HOSTS` / `DISTCC_J`.

## Optional: sccache (default off)

A coordinator-only opt-in (`sccache: true`). Uses sccache local backend with
`actions/cache` (archive-style) persistence, chained outside distcc:
`CC="sccache distcc gcc"` — sccache cache hit skips compilation entirely, misses
fall through to distcc for distributed compile. Kept off by default so the core
"distcc farm stands up" goal is not entangled with cache-backend trade-offs.

## Known Limitations / Risks

- **Open implementation question (must verify first):** tsnet port-forwarding
  carrying distcc end-to-end has NOT been spiked. The CLI+socat+SOCKS5 path was
  verified (fact #1); the tsnet equivalent (`tsnet.Listen`/`tsnet.Dial` bridging
  the coordinator's local port to a worker's distccd) is assumed-good but must be
  proven as the first implementation step, given the spike's lesson that
  unverified assumptions bite (fact #4).
- distcc over DERP relay adds latency (~150ms RTT in the spike); small builds may
  not benefit. Acceptable per non-goals.
- Tailnet peer propagation is eventually consistent; pre-warm + N-consecutive
  confirmation handle the resulting jitter.
- Same-name zombie nodes if `run_id` prefixing is ever bypassed (fact #6).
- A real GitHub Actions cross-host run has not yet been exercised end-to-end;
  the spike modeled it locally via isolated docker networks (fact #3). Final
  confidence requires one real workflow run during implementation.

## Verification / Success Criteria

In a public test repo's Actions: 1 coordinator + 2–3 workers really form a distcc
farm, compile a multi-file C project (e.g. redis) with `distccmon` showing jobs
distributed to workers, the artifact runs, and teardown completes cleanly. Plus
README + tag + example workflow + ACL/OAuth notes so a third party can use it.
