# distcc-action v1

**Borrow CPUs from other GitHub runners to speed up your C/C++ build.**

`distcc-action` turns the runners in a single workflow into a throwaway
[distcc](https://www.distcc.org/) compile farm: one **coordinator** runs your
build, N **worker** runners lend their cores, and your ordinary `make -j` fans
compilation out across all of them. Runners discover and talk to each other over
a temporary [Tailscale](https://tailscale.com/) mesh — no exposed ports, no
servers to keep alive, nothing left running afterwards.

It ships as a single static Go binary (`CGO_ENABLED=0`) that embeds Tailscale via
[`tsnet`](https://tailscale.com/kb/1244/tsnet/) and handles networking,
orchestration, and teardown. Stock `distcc` does the actual compiling.

## Highlights

- **Drop-in.** Add a `workers` matrix job and pass `mode: coordinator` to your
  build job. Your build step stays `make -j${DISTCC_J} CC="${DISTCC_CC_PREFIX} gcc"`.
- **Zero infrastructure.** Discovery and teardown happen over an ephemeral
  Tailscale mesh. No exposed ports, no external KV/database, no manual cleanup.
- **Automatic teardown.** When the coordinator job ends, its node drops from the
  tailnet; workers notice and exit on their own.
- **Fault tolerant.** `min-workers` + `wait-timeout` let the build proceed even
  if some workers are slow to start or never show up.
- **Container-friendly.** Userspace `tsnet` needs no `tailscaled` and no
  `/dev/net/tun`, so the same static binary runs on bare runners or in containers.
- **Live logs.** Each worker streams its `distccd` log into its job output; set
  `DISTCC_VERBOSE=1` on the build to see per-file dispatch.

## Proven on real workloads

CI in the repo exercises the farm end-to-end on GitHub-hosted runners:

- **`selftest`** — builds [redis](https://github.com/redis/redis) across the farm.
- **`smoke`** — builds the **Linux kernel** from `torvalds/linux` and **boots it
  in QEMU** (reaching the init/rootfs stage is the boot-success criterion).
- **`smoke-allyes`** — a 10-worker stress build of the kernel with `allyesconfig`.

## Quick start

```yaml
jobs:
  workers:
    strategy: { fail-fast: false, matrix: { idx: [1, 2, 3] } }
    runs-on: ubuntu-latest
    steps:
      - run: sudo apt-get update && sudo apt-get install -y distcc
      - uses: xdqi/distcc-action@v1
        with:
          mode: worker
          worker-index: ${{ matrix.idx }}
          oauth-secret: ${{ secrets.TS_OAUTH_SECRET }}
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: sudo apt-get update && sudo apt-get install -y distcc
      - uses: xdqi/distcc-action@v1
        with:
          mode: coordinator
          expected-workers: 3
          oauth-secret: ${{ secrets.TS_OAUTH_SECRET }}
      - run: make -j${DISTCC_J} CC="${DISTCC_CC_PREFIX} gcc"
```

Requires a one-time Tailscale OAuth setup — see the [README](../README.md#tailscale-setup-one-time).

## Scope

Same-architecture **native C/C++** (gcc/clang). distcc requires byte-identical
compilers on every node, so this does **not** cover cross-compilation or
`zig cc` / custom wrapper compilers. Best suited to large compile-bound trees;
small projects may not beat a local `-j` due to cross-runner (DERP) latency.

## License

MIT.
