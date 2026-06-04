# distcc-action

Turn N GitHub-hosted runners into an ephemeral [distcc](https://www.distcc.org/)
farm connected by a temporary [Tailscale](https://tailscale.com/) mesh. A single
static Go binary (embedding Tailscale via `tsnet`) handles networking,
orchestration, and teardown; stock `distcc` does the compiling.

> Status: experimental. Built for fun and as a reusable Action. See
> `docs/2026-06-04-distcc-tailnet-action-design.md` for the full design and the
> facts verified during a local spike.

## Scope
Same-architecture **native C/C++** (gcc/clang). NOT for cross-compilation or
`zig cc` / custom wrapper compilers — distcc requires byte-identical compilers
on the coordinator and every worker. Either run all jobs on the same
`ubuntu-latest` (same-epoch images carry the same gcc) or under the same
`container:` image.

## How it works
- One workflow runs 1 coordinator job + N worker jobs (a matrix), all
  GitHub-hosted runners.
- Each runner joins a temporary, ephemeral Tailscale tailnet (via `tsnet`,
  userspace — no system tailscaled needed).
- Workers run `distccd`; the coordinator forwards each worker onto a local port
  and exports `DISTCC_HOSTS`/`DISTCC_J` so your normal `make -j` fans out.
- Teardown is automatic: when the coordinator job ends, its node drops from the
  tailnet; workers notice the coordinator gone and exit on their own.

## Tailscale setup
1. In your tailnet ACL, add the tag under `tagOwners`:
   `"tag:ci-distcc": []`
2. Create an OAuth client (Settings -> OAuth clients) with the `auth_keys`
   write scope and the `tag:ci-distcc` tag.
3. Store the client id and secret as repo secrets, e.g. `TS_OAUTH_ID` and
   `TS_OAUTH_SECRET`.

## Usage
See `examples/build.yml`. Sketch:
- A `workers` job with a matrix; each calls the action with `mode: worker` and a
  unique `worker-index: ${{ matrix.idx }}`.
- A `build` job calling the action with `mode: coordinator` and
  `expected-workers` matching the matrix size, then your build step using
  `make -j${DISTCC_J} CC="${DISTCC_CC_PREFIX} gcc"`.

## Inputs
| input | required | default | description |
|---|---|---|---|
| `mode` | yes | — | `coordinator` or `worker` |
| `oauth-client-id` | no | — | Tailscale OAuth client id (reserved; only the secret is used today) |
| `oauth-secret` | yes | — | Tailscale OAuth client secret |
| `expected-workers` | coordinator | — | workers expected (match the matrix) |
| `min-workers` | no | = expected | min online workers to proceed (else fail after timeout) |
| `wait-timeout` | no | `300s` | max wait for workers |
| `worker-index` | worker | — | unique index per worker (use the matrix value) |
| `tags` | no | `tag:ci-distcc` | tailnet tag(s) to advertise |
| `run-prefix` | no | `github.run_id` | naming prefix (isolates runs, avoids zombie nodes) |
| `distcc-slots` | no | `0` (=nproc) | per-worker job slots |
| `lzo` | no | `true` | enable LZO compression (recommended over DERP) |
| `pump` | no | `false` | enable distcc pump mode (experimental, unverified) |
| `poll-interval` | no | `1s` | teardown poll interval |
| `teardown-threshold` | no | `5` | consecutive offline reads before a worker exits |
| `sccache` | no | `false` | enable the sccache layer |

## Outputs (coordinator)
| output | description |
|---|---|
| `distcc-hosts` | assembled `DISTCC_HOSTS` (also exported to `$GITHUB_ENV`) |
| `distcc-j` | suggested `-j` (sum of worker slots) |
| `workers-online` | number of workers that participated |

The coordinator also exports `DISTCC_J`, `DISTCC_HOSTS`, `DISTCC_WORKERS_ONLINE`,
and `DISTCC_CC_PREFIX` into `$GITHUB_ENV`, so later steps in the same job can use
`$DISTCC_J` / `$DISTCC_CC_PREFIX` directly.

## Notes
- `pump: true` is experimental and unverified under container/cross-host; off by default.
- Teardown latency is ~`teardown-threshold × poll-interval` seconds after the coordinator job ends.
- Build the binary locally with `CGO_ENABLED=0 go build ./cmd/distcc-action` (Go 1.26+; the tailscale dep requires it).

## Known limitations
- Per-host slot count (`distcc-slots=0`) is resolved from the COORDINATOR's CPU count, so on heterogeneous runners (coordinator and workers with different core counts) the slot limit may not match worker capacity. Set `distcc-slots` explicitly for non-uniform runners.
- Designed for GitHub-hosted (ephemeral) runners. On self-hosted persistent runners, the fixed local forward ports (3701+) and the PID file could collide across concurrent jobs on the same machine.
- Early/explicit teardown: the binary supports `./distcc-action --teardown` (kills the detached forwarder) but the composite action does not wire it; teardown normally happens at job end. Advanced users can add their own teardown step.

### sccache / pump
The coordinator exports `DISTCC_CC_PREFIX` (`distcc`, or `sccache distcc` when
`sccache: true`). Use it as `CC="${DISTCC_CC_PREFIX} gcc"`. When `pump: true`,
`DISTCC_PUMP_HINT=1` is also set; wrap the build with the pump script:
`pump make -j${DISTCC_J} CC="${DISTCC_CC_PREFIX} gcc"`.
