# distcc-action

[![smoke](https://github.com/xdqi/distcc-action/actions/workflows/smoke.yml/badge.svg)](https://github.com/xdqi/distcc-action/actions/workflows/smoke.yml)
[![selftest](https://github.com/xdqi/distcc-action/actions/workflows/selftest.yml/badge.svg)](https://github.com/xdqi/distcc-action/actions/workflows/selftest.yml)
[![release](https://img.shields.io/github/v/tag/xdqi/distcc-action?label=release&sort=semver)](https://github.com/xdqi/distcc-action/tags)
[![license](https://img.shields.io/github/license/xdqi/distcc-action)](LICENSE)

**Borrow CPUs from other GitHub runners to speed up your C/C++ build.**

`distcc-action` turns the runners in a single workflow into a throwaway
[distcc](https://www.distcc.org/) compile farm: one **coordinator** runner runs
your build, N **worker** runners lend their cores, and your ordinary `make -j`
fans compilation out across all of them. The runners find and talk to each other
over a temporary [Tailscale](https://tailscale.com/) mesh — no exposed ports, no
servers to keep alive, nothing left running afterwards.

It's a single static Go binary (Tailscale embedded via [`tsnet`](https://tailscale.com/kb/1244/tsnet/))
that does the networking, orchestration, and teardown. Stock `distcc` does the
actual compiling.

> **No Go toolchain required.** This is a **Node 24 JavaScript action** (like
> `actions/setup-go`): a small bundled wrapper runs on the runner's built-in
> Node and, at runtime, downloads the prebuilt static binary for your
> OS/arch from this repo's [Releases](https://github.com/xdqi/distcc-action/releases)
> (cached via the runner tool-cache). The version is resolved automatically from
> the `@vX`/`@vX.Y.Z` ref in your `uses:` line. If the download is unavailable
> and `go` happens to be on `PATH`, it falls back to building from source.

> **Proof it works:** the CI in this repo uses the farm to build the **Linux
> kernel** from `torvalds/linux` and boots the result in QEMU. (see the [smoke workflow](.github/workflows/smoke.yml))

---

## Contents

- [Quick start](#quick-start)
- [How it works](#how-it-works)
- [Tailscale setup (one time)](#tailscale-setup-one-time)
- [Inputs](#inputs)
- [Outputs](#outputs)
- [Recipes](#recipes)
- [Scope & limitations](#scope--limitations)
- [FAQ](#faq)

---

## Quick start

Two jobs in one workflow: a `workers` matrix that lends cores, and a `build` job
that compiles using them.

```yaml
name: build
on: [push]

jobs:
  workers:
    strategy:
      fail-fast: false
      matrix: { idx: [1, 2, 3] }   # 3 helper runners
    runs-on: ubuntu-latest
    steps:
      - uses: xdqi/distcc-action@v1
        with:
          mode: worker
          worker-index: ${{ matrix.idx }}
          install-distcc: true   # or install distcc yourself
          oauth-client-id: ${{ secrets.TS_OAUTH_ID }}
          oauth-secret:    ${{ secrets.TS_OAUTH_SECRET }}

  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: xdqi/distcc-action@v1
        with:
          mode: coordinator
          expected-workers: 3
          install-distcc: true
          oauth-client-id: ${{ secrets.TS_OAUTH_ID }}
          oauth-secret:    ${{ secrets.TS_OAUTH_SECRET }}
      - name: Build
        run: make -j${DISTCC_J} CC="${DISTCC_CC_PREFIX} gcc"
```

That's it. The coordinator exports `DISTCC_HOSTS`, `DISTCC_J`, and
`DISTCC_CC_PREFIX` into the job environment, so the build step just uses them.
Teardown is automatic when the coordinator job ends.

> Requires a one-time [Tailscale setup](#tailscale-setup-one-time). The `distcc`
> package must be on every runner — pass `install-distcc: true` to have the
> action install it (apt/Linux), or install it yourself.

---

## How it works

```
            workflow
   ┌───────────────────────────────────────────────┐
   │  workers (matrix)            build (coordinator)│
   │  ┌──────────┐                ┌────────────────┐ │
   │  │ worker-1 │  distccd       │  your make -j  │ │
   │  │ worker-2 │◄──────────────►│  CC=distcc gcc │ │
   │  │ worker-3 │   Tailscale    │                │ │
   │  └──────────┘   mesh (tsnet) └────────────────┘ │
   └───────────────────────────────────────────────┘
```

1. Every runner joins a temporary, **ephemeral** Tailscale tailnet (userspace
   `tsnet` — no system `tailscaled`, no `/dev/net/tun` needed).
2. Workers start `distccd` and expose it on the tailnet. The coordinator
   discovers them by hostname, forwards each onto a local port, and exports
   `DISTCC_HOSTS` / `DISTCC_J`.
3. Your build runs `make -j$DISTCC_J CC="distcc gcc"` — distcc ships each
   translation unit to a worker, compiles remotely, returns the `.o`.
4. **Teardown is automatic:** when the coordinator job finishes, its Tailscale
   node drops; workers notice it's gone and exit on their own. Nothing lingers.

Discovery and teardown happen entirely over the mesh — no external service,
no database, no manual cleanup. Per-run hostnames are namespaced by
`github.run_id`, so concurrent runs never collide.

---

## Tailscale setup (one time)

You need a (free) Tailscale account and an OAuth client for CI.

1. **ACL** — in your [tailnet policy file](https://login.tailscale.com/admin/acls),
   declare the tag under `tagOwners`:
   ```jsonc
   "tagOwners": {
     "tag:ci-distcc": []
   }
   ```
2. **OAuth client** — at [Settings → OAuth clients](https://login.tailscale.com/admin/settings/oauth),
   create one with the **`auth_keys`** write scope and the **`tag:ci-distcc`** tag.
   (OAuth clients don't expire on the 90-day auth-key rotation — ideal for CI.)
3. **Secrets** — store the client secret as a repo secret, e.g.
   `TS_OAUTH_SECRET`. (`TS_OAUTH_ID` is accepted but currently unused.)

Nodes are created **ephemeral** and tagged `tag:ci-distcc`; they auto-remove
from your tailnet after each run.

---

## Inputs

| Input | Required | Default | Description |
|---|---|---|---|
| `mode` | **yes** | — | `coordinator` or `worker` |
| `oauth-secret` | **yes** | — | Tailscale OAuth client secret (used as the ephemeral auth key) |
| `oauth-client-id` | no | — | Accepted for forward-compat; **currently unused** |
| `tags` | no | `tag:ci-distcc` | Tailnet tag(s) to advertise (must exist in your ACL `tagOwners`) |
| `expected-workers` | coordinator | — | Number of workers to wait for (match your matrix size) |
| `min-workers` | no | = `expected-workers` | Proceed once this many are online; fewer (after timeout) fails the build |
| `wait-timeout` | no | `300s` | Max time the coordinator waits for workers |
| `worker-index` | **worker** | — | Unique index per worker — pass `${{ matrix.idx }}` |
| `run-prefix` | no | `${{ github.run_id }}` | Namespacing prefix for node names (avoids cross-run collisions) |
| `distcc-slots` | no | `0` (= nproc) | Concurrent jobs per worker (the `/N` in `DISTCC_HOSTS`) |
| `lzo` | no | `true` | LZO-compress traffic (recommended over the mesh) |
| `pump` | no | `false` | distcc pump mode — **experimental, unverified**; see [limitations](#scope--limitations) |
| `sccache` | no | `false` | Wrap distcc with [sccache](https://github.com/mozilla/sccache) (sets `DISTCC_CC_PREFIX` to `sccache distcc`) |
| `poll-interval` | no | `1s` | How often a worker polls for the coordinator during teardown |
| `teardown-threshold` | no | `5` | Consecutive "coordinator gone" reads before a worker exits |
| `distcc-log-level` | no | `info` | Worker `distccd` log level: `critical`…`debug` (logs stream live into the worker job) |
| `install-distcc` | no | `false` | Install the stock `distcc` package for you (apt, Linux only). Leave `false` to install it yourself (e.g. pinned version, non-apt distro, container with it preinstalled). |

## Outputs

Set on the **coordinator** step (e.g. `steps.<id>.outputs.distcc-hosts`):

| Output | Description |
|---|---|
| `distcc-hosts` | The assembled `DISTCC_HOSTS` string |
| `distcc-j` | Suggested `-j` value (total worker slots) |
| `workers-online` | Number of workers that actually joined |

The coordinator **also** exports these into `$GITHUB_ENV` for later steps in the
same job, so you usually don't touch the outputs directly:

| Env var | Use |
|---|---|
| `DISTCC_HOSTS` | distcc reads it automatically |
| `DISTCC_J` | `make -j${DISTCC_J}` |
| `DISTCC_CC_PREFIX` | `CC="${DISTCC_CC_PREFIX} gcc"` (`distcc`, or `sccache distcc` when `sccache: true`) |
| `DISTCC_WORKERS_ONLINE` | how many workers joined |

---

## Recipes

**CMake**

```yaml
- run: cmake -B build -DCMAKE_C_COMPILER_LAUNCHER="distcc"
- run: cmake --build build -j${DISTCC_J}
```

**Tolerate slow/absent workers** — proceed as soon as half show up:

```yaml
with:
  mode: coordinator
  expected-workers: 10
  min-workers: 5
  wait-timeout: 420s
```

**sccache + distcc** — cache hits skip compilation, misses fan out to the farm:

```yaml
with: { mode: coordinator, expected-workers: 3, sccache: true, ... }
# then:
- run: make -j${DISTCC_J} CC="${DISTCC_CC_PREFIX} gcc"
```

**Watch the distribution** — set `DISTCC_VERBOSE=1` on your build to see each
file's target host; or read each worker job's live `distccd` log.

See [`examples/build.yml`](examples/build.yml) and the [smoke](.github/workflows/smoke.yml) /
[selftest](.github/workflows/selftest.yml) workflows for complete, working setups.

---

## Scope & limitations

- **Same-architecture native C/C++ only** (gcc/clang). distcc requires
  byte-identical compilers on the coordinator and every worker, so this does
  **not** work for cross-compilation or `zig cc` / custom wrapper compilers.
  Keep all jobs on the same `ubuntu-latest` (same-epoch images carry the same
  gcc) or under one shared `container:` image.
- **Not always faster.** Traffic between runners may traverse a Tailscale DERP
  relay (~150 ms RTT). Small projects can be slower than a local `-j`; the win
  shows up on large compile-bound trees (kernels, LLVM, big C++ apps).
- **`pump: true` is experimental** and unverified under the container/cross-host
  path; it's off by default. When enabled, the coordinator also exports
  `DISTCC_PUMP_HINT=1` — wrap your build with the pump script:
  `pump make -j${DISTCC_J} CC="${DISTCC_CC_PREFIX} gcc"`.
- **Hosted runners assumed.** The fixed local ports and per-run state suit
  ephemeral GitHub-hosted runners. On persistent self-hosted runners, concurrent
  jobs on one machine could collide — set distinct `run-prefix` values.
- **Slot count** (`distcc-slots=0`) is taken from the *coordinator's* CPU count.
  On heterogeneous runners, set it explicitly to match worker cores.

---

## FAQ

**Do I need to expose any ports or run a server?**
No. All communication is over the ephemeral Tailscale mesh; nodes are removed
after the run.

**What happens if a worker is slow to start or never starts?**
The coordinator waits up to `wait-timeout`, then proceeds if at least
`min-workers` are online (defaults to `expected-workers`). Set `min-workers`
lower to tolerate stragglers.

**How is teardown handled?**
When the coordinator job ends, its node leaves the tailnet. Each worker polls
and exits after `teardown-threshold × poll-interval` seconds. No explicit step
needed. (The binary also has a `--teardown` entrypoint for early/explicit
teardown, if you want to wire your own step.)

**Does it leak anything into my tailnet?**
Nodes are ephemeral and tagged `tag:ci-distcc`; Tailscale removes them shortly
after the job finishes.

**Why Go + tsnet instead of the Tailscale CLI?**
`tsnet` embeds a userspace Tailscale node directly in the binary — no
`tailscaled`, no `/dev/net/tun`, no SOCKS proxy plumbing. One static
(`CGO_ENABLED=0`) binary runs anywhere, including inside containers.

**Do I need to install Go?**
No. The action is a Node 24 JS wrapper that downloads the prebuilt binary from
this repo's Releases at runtime — only the runner's built-in Node is used. The
binary version is picked from your `uses:` ref: `@v1` resolves to the latest
`v1.*` release, `@v1.2.3` to exactly that tag. (Prebuilt assets are published
for Linux `amd64`/`arm64`/`arm`/`386` and macOS `amd64`/`arm64`; Windows is not
supported — distcc is a POSIX tool.) Only this repo's own CI — which pins
`uses: ./` to test the current
checkout — builds the binary from source, and that's why those workflows install
Go.

---

## Design

The full design and the engineering decisions (including a discarded teardown
mechanism and the facts verified during a local spike) are documented in
[`docs/`](docs/).

## License

See [LICENSE](LICENSE).
