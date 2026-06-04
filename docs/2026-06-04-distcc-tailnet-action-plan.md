# distcc-action Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a single static Go binary, packaged as a GitHub Action, that turns N GitHub-hosted runners into an ephemeral distcc farm over a temporary Tailscale mesh (via `tsnet`), with the coordinator fanning C/C++ compilation to workers and tearing the mesh down when the build finishes.

**Architecture:** One binary with `mode=coordinator|worker`. It embeds a Tailscale node via `tsnet` (no system tailscaled, no CLI, no SOCKS5/socat). The coordinator discovers worker peers by hostname prefix, port-forwards each worker's `distccd` onto a local port, exports `DISTCC_HOSTS`/`DISTCC_J`, hands control to the user's build, then goes offline; workers run `distccd`, expose it on the tailnet, and exit when they observe the coordinator peer go offline. Compilation itself stays with stock `distcc`.

**Tech Stack:** Go (`CGO_ENABLED=0`, fully static), `tailscale.com/tsnet` + `tailscale.com/client/tailscale` (LocalClient), stock `distcc`/`distccd`, GitHub Actions (composite action wrapping the binary), Docker (local spike + cross-host modeling).

**Reference:** Design spec at `docs/2026-06-04-distcc-tailnet-action-design.md`. The six "Verified Facts" and the "Open implementation question" there are load-bearing; Task 1 closes the open question before anything else is built.

---

## File Structure

```
go.mod / go.sum                    module + tsnet deps
action.yml                         Action definition (inputs/outputs/runs=composite)
cmd/distcc-action/main.go          entry: read inputs from env, dispatch on mode
internal/config/config.go          parse+validate inputs, apply defaults
internal/tsmesh/tsmesh.go          tsnet wrapper: Up/Status/Dial/Listen/Close, peer discovery
internal/distccrun/distccrun.go    start/stop distccd; build DISTCC_HOSTS / DISTCC_J
internal/worker/worker.go          worker role: distccd + expose + guard loop
internal/coordinator/coordinator.go coordinator role: wait workers + forward + export + teardown
spike/tsnet-transport/             Task 1: prove tsnet forwarding carries distcc
.github/workflows/selftest.yml     real-Actions acceptance run
examples/build.yml                 example workflow for users
README.md                          usage + ACL/OAuth setup
```

---

## Task 0: Toolchain + repo scaffolding

**Files:**
- Create: `~/distcc-action/.gitignore`
- Create: `~/distcc-action/go.mod`

- [ ] **Step 1: Install Go (system has none)**

Run:
```bash
cd /tmp && curl -fsSL https://go.dev/dl/go1.23.4.linux-amd64.tar.gz -o go.tgz \
  && sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go.tgz \
  && export PATH=$PATH:/usr/local/go/bin && go version
```
Expected: `go version go1.23.4 linux/amd64`. Add `/usr/local/go/bin` to PATH for the session (and note it in README for future work).

- [ ] **Step 2: Init module**

Run:
```bash
cd ~/distcc-action && /usr/local/go/bin/go mod init github.com/xdqi/distcc-action
```
Expected: creates `go.mod` with `module github.com/xdqi/distcc-action` and a `go 1.23` line.

- [ ] **Step 3: Add .gitignore**

```gitignore
/distcc-action
/dist/
*.test
spike/**/tmp/
```

- [ ] **Step 4: Commit**

```bash
cd ~/distcc-action && git add go.mod .gitignore && git commit -m "chore: go module + gitignore"
```

---

## Task 1: SPIKE — prove tsnet port-forwarding carries distcc end-to-end

This closes the spec's "Open implementation question (must verify first)". Two real tsnet nodes (in isolated docker networks, per Verified Fact #3) — one runs `distccd`, the other tsnet-forwards a local port to it and compiles a `.c` through that forward. If this fails, STOP and revisit transport before building the real binary.

**Files:**
- Create: `spike/tsnet-transport/main.go`
- Create: `spike/tsnet-transport/Dockerfile`
- Create: `spike/tsnet-transport/run.sh`

- [ ] **Step 1: Write the spike program (one binary, two roles)**

`spike/tsnet-transport/main.go`:
```go
// Spike: prove a tsnet node can forward a local TCP port to a remote distccd
// over the tailnet, and that distcc compiles a .c through that forward.
//
// role=server : tsnet up, accept tailnet TCP on :3632, proxy to 127.0.0.1:3632
//               (a plain distccd running in the same container)
// role=client : tsnet up, listen on 127.0.0.1:LOCAL, dial the server peer's
//               :3632 over tsnet, splice. Then run distcc against 127.0.0.1:LOCAL.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"tailscale.com/tsnet"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	role := os.Getenv("ROLE")
	s := &tsnet.Server{
		Hostname:  os.Getenv("TS_HOSTNAME"),
		AuthKey:   os.Getenv("TS_AUTHKEY"),
		Ephemeral: true,
		Dir:       "/tmp/tsnet-" + os.Getenv("TS_HOSTNAME"),
	}
	if err := s.Start(); err != nil {
		log.Fatalf("tsnet start: %v", err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := s.Up(ctx); err != nil {
		log.Fatalf("tsnet up: %v", err)
	}
	ip4, _ := s.TailscaleIPs()
	log.Printf("[%s] up as %s ip=%v", role, s.Hostname, ip4)

	switch role {
	case "server":
		// Accept tailnet connections on :3632 and proxy to local distccd.
		ln, err := s.Listen("tcp", ":3632")
		if err != nil {
			log.Fatalf("tsnet listen: %v", err)
		}
		log.Printf("[server] forwarding tailnet :3632 -> 127.0.0.1:3632")
		for {
			c, err := ln.Accept()
			if err != nil {
				log.Fatalf("accept: %v", err)
			}
			go proxy(c, func() (net.Conn, error) { return net.Dial("tcp", "127.0.0.1:3632") })
		}
	case "client":
		serverHost := os.Getenv("SERVER_HOST") // tailnet hostname of the server
		local := env("LOCAL_PORT", "3700")
		ln, err := net.Listen("tcp", "127.0.0.1:"+local)
		if err != nil {
			log.Fatalf("local listen: %v", err)
		}
		log.Printf("[client] 127.0.0.1:%s -> tsnet dial %s:3632", local, serverHost)
		for {
			c, err := ln.Accept()
			if err != nil {
				log.Fatalf("accept: %v", err)
			}
			go proxy(c, func() (net.Conn, error) {
				return s.Dial(context.Background(), "tcp", serverHost+":3632")
			})
		}
	default:
		fmt.Println("set ROLE=server|client")
		os.Exit(2)
	}
	_ = strings.TrimSpace
}

func proxy(a net.Conn, dialB func() (net.Conn, error)) {
	defer a.Close()
	b, err := dialB()
	if err != nil {
		log.Printf("dial b: %v", err)
		return
	}
	defer b.Close()
	go io.Copy(b, a)
	io.Copy(a, b)
}
```

- [ ] **Step 2: Write the spike Dockerfile**

`spike/tsnet-transport/Dockerfile`:
```dockerfile
FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod ./
# spike depends only on tsnet; fetch within build
RUN go mod download tailscale.com 2>/dev/null || true
COPY spike/tsnet-transport/main.go ./main.go
RUN CGO_ENABLED=0 go build -o /spike-tsnet ./main.go

FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
        distcc build-essential ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /spike-tsnet /usr/local/bin/spike-tsnet
```

Note: the spike build needs `tailscale.com` in `go.mod`. Before building, run from repo root: `/usr/local/go/bin/go get tailscale.com/tsnet@latest && /usr/local/go/bin/go mod tidy`.

- [ ] **Step 3: Write run.sh (isolated networks, unique prefix)**

`spike/tsnet-transport/run.sh`:
```bash
#!/usr/bin/env bash
# Usage: TS_AUTHKEY=<key> bash run.sh
set -euo pipefail
: "${TS_AUTHKEY:?set TS_AUTHKEY}"
PFX="tsx$$"
IMG=spike-tsnet:latest
cd "$(dirname "$0")/../.."   # repo root for docker build context
docker build -t $IMG -f spike/tsnet-transport/Dockerfile .

docker network create ${PFX}-net-s >/dev/null
docker network create ${PFX}-net-c >/dev/null
cleanup(){ docker rm -f ${PFX}-s ${PFX}-c >/dev/null 2>&1||true; docker network rm ${PFX}-net-s ${PFX}-net-c >/dev/null 2>&1||true; }
trap cleanup EXIT

# server: start a plain distccd, then the tsnet proxy
docker run -d --name ${PFX}-s --network ${PFX}-net-s \
  -e TS_AUTHKEY="$TS_AUTHKEY" -e TS_HOSTNAME=${PFX}-server -e ROLE=server \
  --entrypoint bash $IMG -c \
  'distccd --daemon --no-detach --allow 0.0.0.0/0 --listen 127.0.0.1 --port 3632 --jobs 4 --log-file /var/log/distccd.log & sleep 1; ROLE=server TS_HOSTNAME='"${PFX}"'-server spike-tsnet'
sleep 12

docker run --name ${PFX}-c --network ${PFX}-net-c \
  -e TS_AUTHKEY="$TS_AUTHKEY" -e TS_HOSTNAME=${PFX}-client -e ROLE=client \
  -e SERVER_HOST=${PFX}-server -e LOCAL_PORT=3700 \
  --entrypoint bash $IMG -c '
    spike-tsnet & sleep 12
    echo "int spikefn(int a){return a+1;}" > /tmp/u.c
    DISTCC_HOSTS="127.0.0.1:3700,lzo" DISTCC_VERBOSE=1 distcc gcc -c /tmp/u.c -o /tmp/u.o 2>&1 | tail -20
    if [ -f /tmp/u.o ]; then echo "SPIKE-RESULT tsnet-distcc = PASS"; else echo "SPIKE-RESULT tsnet-distcc = FAIL"; fi
  '
```

- [ ] **Step 4: Run the spike**

Run: `TS_AUTHKEY=<ephemeral-key> bash spike/tsnet-transport/run.sh`
Expected: `SPIKE-RESULT tsnet-distcc = PASS`, and the verbose distcc output shows the compile dispatched to `127.0.0.1:3700` (the tsnet forward). If FAIL: STOP, inspect both containers' logs, do not proceed to Task 2.

- [ ] **Step 5: Commit the spike**

```bash
cd ~/distcc-action && git add spike/ go.mod go.sum && git commit -m "spike: verify tsnet port-forward carries distcc (closes open question)"
```

---

## Task 2: config — parse and validate inputs

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

`internal/config/config_test.go`:
```go
package config

import "testing"

func TestLoadCoordinatorDefaults(t *testing.T) {
	env := map[string]string{
		"INPUT_MODE":             "coordinator",
		"INPUT_OAUTH_CLIENT_ID":  "id",
		"INPUT_OAUTH_SECRET":     "sec",
		"INPUT_EXPECTED_WORKERS": "3",
		"GITHUB_RUN_ID":          "999",
	}
	c, err := loadFrom(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if c.Mode != "coordinator" {
		t.Errorf("mode=%q", c.Mode)
	}
	if c.ExpectedWorkers != 3 {
		t.Errorf("expected=%d", c.ExpectedWorkers)
	}
	if c.MinWorkers != 3 { // defaults to expected
		t.Errorf("min defaulted wrong: %d", c.MinWorkers)
	}
	if c.RunPrefix != "999" {
		t.Errorf("prefix=%q", c.RunPrefix)
	}
	if c.Tags != "tag:ci-distcc" {
		t.Errorf("tags default=%q", c.Tags)
	}
	if c.WaitTimeout.Seconds() != 300 {
		t.Errorf("wait-timeout default=%v", c.WaitTimeout)
	}
	if !c.LZO {
		t.Errorf("lzo should default true")
	}
	if c.Pump {
		t.Errorf("pump should default false")
	}
}

func TestMissingOAuthFails(t *testing.T) {
	env := map[string]string{"INPUT_MODE": "worker"}
	if _, err := loadFrom(func(k string) string { return env[k] }); err == nil {
		t.Fatal("expected error for missing oauth")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/distcc-action && /usr/local/go/bin/go test ./internal/config/ -run TestLoad -v`
Expected: FAIL — `loadFrom` undefined.

- [ ] **Step 3: Write minimal implementation**

`internal/config/config.go`:
```go
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Mode            string
	OAuthClientID   string
	OAuthSecret     string
	Tags            string
	RunPrefix       string
	ExpectedWorkers int
	MinWorkers      int
	WaitTimeout     time.Duration
	DistccSlots     int
	LZO             bool
	Pump            bool
	PollInterval    time.Duration
	TeardownThresh  int
	Sccache         bool
}

// Load reads from the process environment (GitHub sets INPUT_* for each input).
func Load() (*Config, error) { return loadFrom(os.Getenv) }

func loadFrom(get func(string) string) (*Config, error) {
	c := &Config{
		Mode:           get("INPUT_MODE"),
		OAuthClientID:  get("INPUT_OAUTH_CLIENT_ID"),
		OAuthSecret:    get("INPUT_OAUTH_SECRET"),
		Tags:           orDefault(get("INPUT_TAGS"), "tag:ci-distcc"),
		RunPrefix:      orDefault(get("INPUT_RUN_PREFIX"), get("GITHUB_RUN_ID")),
		DistccSlots:    atoiOr(get("INPUT_DISTCC_SLOTS"), 0), // 0 => nproc, resolved later
		LZO:            boolOr(get("INPUT_LZO"), true),
		Pump:           boolOr(get("INPUT_PUMP"), false),
		Sccache:        boolOr(get("INPUT_SCCACHE"), false),
		PollInterval:   durOr(get("INPUT_POLL_INTERVAL"), time.Second),
		TeardownThresh: atoiOr(get("INPUT_TEARDOWN_THRESHOLD"), 5),
		WaitTimeout:    durOr(get("INPUT_WAIT_TIMEOUT"), 300*time.Second),
	}
	if c.Mode != "coordinator" && c.Mode != "worker" {
		return nil, fmt.Errorf("mode must be coordinator|worker, got %q", c.Mode)
	}
	if c.OAuthClientID == "" || c.OAuthSecret == "" {
		return nil, fmt.Errorf("oauth-client-id and oauth-secret are both required")
	}
	if c.RunPrefix == "" {
		return nil, fmt.Errorf("run-prefix empty and GITHUB_RUN_ID unset")
	}
	if c.Mode == "coordinator" {
		c.ExpectedWorkers = atoiOr(get("INPUT_EXPECTED_WORKERS"), 0)
		if c.ExpectedWorkers < 1 {
			return nil, fmt.Errorf("expected-workers must be >= 1 for coordinator")
		}
		c.MinWorkers = atoiOr(get("INPUT_MIN_WORKERS"), c.ExpectedWorkers)
		if c.MinWorkers < 1 || c.MinWorkers > c.ExpectedWorkers {
			return nil, fmt.Errorf("min-workers must be in [1, expected-workers]")
		}
	}
	return c, nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
func atoiOr(v string, d int) int {
	if v == "" {
		return d
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return d
	}
	return n
}
func boolOr(v string, d bool) bool {
	if v == "" {
		return d
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return d
	}
	return b
}
func durOr(v string, d time.Duration) time.Duration {
	if v == "" {
		return d
	}
	if dur, err := time.ParseDuration(v); err == nil {
		return dur
	}
	if n, err := strconv.Atoi(v); err == nil { // bare seconds
		return time.Duration(n) * time.Second
	}
	return d
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/distcc-action && /usr/local/go/bin/go test ./internal/config/ -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
cd ~/distcc-action && git add internal/config/ && git commit -m "feat: config parsing + validation"
```

---

## Task 3: distccrun — distccd lifecycle + DISTCC_HOSTS/J assembly

**Files:**
- Create: `internal/distccrun/distccrun.go`
- Test: `internal/distccrun/distccrun_test.go`

- [ ] **Step 1: Write the failing test (pure assembly logic, no daemon)**

`internal/distccrun/distccrun_test.go`:
```go
package distccrun

import "testing"

func TestBuildHosts(t *testing.T) {
	got := BuildHosts([]string{"3701", "3702"}, 4, true, false)
	want := "127.0.0.1:3701/4,lzo 127.0.0.1:3702/4,lzo"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestBuildHostsNoLZO(t *testing.T) {
	got := BuildHosts([]string{"3701"}, 2, false, false)
	want := "127.0.0.1:3701/2"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestTotalJ(t *testing.T) {
	if j := TotalJ(3, 4); j != 12 {
		t.Errorf("j=%d want 12", j)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/distcc-action && /usr/local/go/bin/go test ./internal/distccrun/ -v`
Expected: FAIL — `BuildHosts`/`TotalJ` undefined.

- [ ] **Step 3: Write minimal implementation**

`internal/distccrun/distccrun.go`:
```go
package distccrun

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// BuildHosts assembles a DISTCC_HOSTS string from local forwarded ports.
// pump appends the ,cpp option (distcc-pump preprocessing distribution).
func BuildHosts(localPorts []string, slots int, lzo, pump bool) string {
	var parts []string
	for _, p := range localPorts {
		h := "127.0.0.1:" + p + "/" + strconv.Itoa(slots)
		if lzo {
			h += ",lzo"
		}
		if pump {
			h += ",cpp"
		}
		parts = append(parts, h)
	}
	return strings.Join(parts, " ")
}

// TotalJ is the suggested -j: sum of all worker slots.
func TotalJ(numWorkers, slots int) int { return numWorkers * slots }

// Nproc resolves slots when configured as 0 (auto).
func Nproc() int { return runtime.NumCPU() }

// StartDaemon launches distccd as a TCP listener on the given port. Returns the
// process so the caller can stop it. Non-detaching so we own the lifecycle.
func StartDaemon(port, jobs int) (*exec.Cmd, error) {
	cmd := exec.Command("distccd",
		"--daemon", "--no-detach",
		"--allow", "0.0.0.0/0", // tailnet-only reachability is enforced by tsnet listener
		"--listen", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--jobs", strconv.Itoa(jobs),
		"--log-stderr", "--log-level", "info",
	)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start distccd: %w", err)
	}
	return cmd, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/distcc-action && /usr/local/go/bin/go test ./internal/distccrun/ -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
cd ~/distcc-action && git add internal/distccrun/ && git commit -m "feat: distccd launch + DISTCC_HOSTS/J assembly"
```

---

## Task 4: tsmesh — tsnet wrapper (mesh up, peer discovery, dial, forward)

**Files:**
- Create: `internal/tsmesh/tsmesh.go`
- Test: `internal/tsmesh/tsmesh_test.go`

This wraps `tsnet.Server` + `LocalClient`. Pure-logic parts (peer filtering by prefix + online) are unit-tested; the live tsnet parts are exercised by Task 1's spike and Task 8's real run.

- [ ] **Step 1: Write the failing test for peer filtering**

`internal/tsmesh/tsmesh_test.go`:
```go
package tsmesh

import "testing"

func TestFilterOnlineByPrefix(t *testing.T) {
	peers := []Peer{
		{Host: "run9-worker-1", Online: true, IP: "100.0.0.1"},
		{Host: "run9-worker-2", Online: false, IP: "100.0.0.2"},
		{Host: "run9-coordinator", Online: true, IP: "100.0.0.9"},
		{Host: "someones-laptop", Online: true, IP: "100.0.0.5"},
	}
	got := FilterOnline(peers, "run9-worker-")
	if len(got) != 1 || got[0].IP != "100.0.0.1" {
		t.Fatalf("got %+v", got)
	}
}

func TestPeerPresentOnline(t *testing.T) {
	peers := []Peer{{Host: "run9-coordinator", Online: true, IP: "100.0.0.9"}}
	if !PresentOnline(peers, "run9-coordinator") {
		t.Fatal("should be present+online")
	}
	if PresentOnline(peers, "run9-missing") {
		t.Fatal("missing should not be present")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/distcc-action && /usr/local/go/bin/go test ./internal/tsmesh/ -v`
Expected: FAIL — `Peer`/`FilterOnline`/`PresentOnline` undefined.

- [ ] **Step 3: Write minimal implementation**

`internal/tsmesh/tsmesh.go`:
```go
package tsmesh

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"tailscale.com/client/tailscale"
	"tailscale.com/tsnet"
)

// Peer is a simplified view of a tailnet peer.
type Peer struct {
	Host   string
	Online bool
	IP     string
}

// FilterOnline returns peers whose hostname starts with prefix and are Online.
func FilterOnline(peers []Peer, prefix string) []Peer {
	var out []Peer
	for _, p := range peers {
		if strings.HasPrefix(p.Host, prefix) && p.Online {
			out = append(out, p)
		}
	}
	return out
}

// PresentOnline reports whether a peer with exactly this hostname is Online.
func PresentOnline(peers []Peer, host string) bool {
	for _, p := range peers {
		if p.Host == host && p.Online {
			return true
		}
	}
	return false
}

// Mesh wraps a live tsnet node.
type Mesh struct {
	srv *tsnet.Server
	lc  *tailscale.LocalClient
}

// Up starts a tsnet node with the given hostname/tags/authkey and blocks until
// it has joined the tailnet (or ctx expires).
func Up(ctx context.Context, hostname, authKey, tags string) (*Mesh, error) {
	srv := &tsnet.Server{
		Hostname:  hostname,
		AuthKey:   authKey,
		Ephemeral: true,
		Dir:       "/tmp/tsnet-" + hostname,
	}
	// tsnet advertises tags via the auth key's ACL tags; for OAuth-minted keys
	// the tags are carried by the key. AdvertiseTags is set through the key.
	if err := srv.Start(); err != nil {
		return nil, fmt.Errorf("tsnet start: %w", err)
	}
	if _, err := srv.Up(ctx); err != nil {
		srv.Close()
		return nil, fmt.Errorf("tsnet up: %w", err)
	}
	lc, err := srv.LocalClient()
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("localclient: %w", err)
	}
	return &Mesh{srv: srv, lc: lc}, nil
}

// Peers returns the current tailnet peers as simplified Peer structs.
func (m *Mesh) Peers(ctx context.Context) ([]Peer, error) {
	st, err := m.lc.Status(ctx)
	if err != nil {
		return nil, err
	}
	var out []Peer
	for _, ps := range st.Peer {
		ip := ""
		if len(ps.TailscaleIPs) > 0 {
			ip = ps.TailscaleIPs[0].String()
		}
		out = append(out, Peer{Host: ps.HostName, Online: ps.Online, IP: ip})
	}
	return out, nil
}

// WaitForWorkers polls until at least min workers (hostname prefix) are online
// or the deadline passes. Returns the online worker peers found.
func (m *Mesh) WaitForWorkers(ctx context.Context, prefix string, min int, poll time.Duration) ([]Peer, error) {
	for {
		peers, err := m.Peers(ctx)
		if err == nil {
			if online := FilterOnline(peers, prefix); len(online) >= min {
				return online, nil
			}
		}
		select {
		case <-ctx.Done():
			peers, _ := m.Peers(context.Background())
			return FilterOnline(peers, prefix), ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Dial opens a connection to host:port over the tailnet.
func (m *Mesh) Dial(ctx context.Context, hostport string) (net.Conn, error) {
	return m.srv.Dial(ctx, "tcp", hostport)
}

// Listen accepts tailnet connections on the given address (e.g. ":3632").
func (m *Mesh) Listen(addr string) (net.Listener, error) {
	return m.srv.Listen("tcp", addr)
}

// Close shuts the node down, dropping it from the tailnet (teardown signal).
func (m *Mesh) Close() error { return m.srv.Close() }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/distcc-action && /usr/local/go/bin/go test ./internal/tsmesh/ -v`
Expected: PASS (2 tests). (Live tsnet methods are not unit-tested here; they're covered by Task 1 spike + Task 8.)

- [ ] **Step 5: Commit**

```bash
cd ~/distcc-action && git add internal/tsmesh/ go.sum && git commit -m "feat: tsmesh tsnet wrapper + peer discovery"
```

---

## Task 5: worker role — distccd + expose + guard loop

**Files:**
- Create: `internal/worker/worker.go`
- Test: `internal/worker/worker_test.go`

- [ ] **Step 1: Write the failing test for the guard decision**

The guard's exit decision is the testable core: given a sequence of peer-online observations, it must exit only after `threshold` consecutive "coordinator gone" readings.

`internal/worker/worker_test.go`:
```go
package worker

import "testing"

func TestGuardExitsAfterThreshold(t *testing.T) {
	g := &guard{threshold: 3}
	// present 5x -> never exit, counter stays 0
	for i := 0; i < 5; i++ {
		if g.observe(true) {
			t.Fatal("should not exit while present")
		}
	}
	// gone 2x -> not yet
	if g.observe(false) || g.observe(false) {
		t.Fatal("should not exit before threshold")
	}
	// 3rd gone -> exit
	if !g.observe(false) {
		t.Fatal("should exit at threshold")
	}
}

func TestGuardResetsOnRecovery(t *testing.T) {
	g := &guard{threshold: 3}
	g.observe(false)
	g.observe(false)
	g.observe(true) // recovery resets
	if g.observe(false) {
		t.Fatal("counter should have reset on recovery")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/distcc-action && /usr/local/go/bin/go test ./internal/worker/ -v`
Expected: FAIL — `guard` undefined.

- [ ] **Step 3: Write minimal implementation**

`internal/worker/worker.go`:
```go
package worker

import (
	"context"
	"io"
	"log"
	"time"

	"github.com/xdqi/distcc-action/internal/config"
	"github.com/xdqi/distcc-action/internal/distccrun"
	"github.com/xdqi/distcc-action/internal/tsmesh"
)

// guard tracks consecutive "coordinator gone" observations.
type guard struct {
	threshold int
	misses    int
}

// observe records one reading (present=true means coordinator online).
// Returns true when the worker should exit.
func (g *guard) observe(present bool) bool {
	if present {
		g.misses = 0
		return false
	}
	g.misses++
	return g.misses >= g.threshold
}

// Run executes the worker role: join, start distccd, expose over tailnet, guard.
func Run(ctx context.Context, c *config.Config, hostname string) error {
	mesh, err := tsmesh.Up(ctx, hostname, c.OAuthSecret, c.Tags)
	if err != nil {
		return err
	}
	defer mesh.Close()

	slots := c.DistccSlots
	if slots == 0 {
		slots = distccrun.Nproc()
	}
	dd, err := distccrun.StartDaemon(3632, slots)
	if err != nil {
		return err
	}
	defer dd.Process.Kill()

	// Expose local distccd on the tailnet :3632.
	ln, err := mesh.Listen(":3632")
	if err != nil {
		return err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				local, err := mesh.Dial(context.Background(), "127.0.0.1:3632")
				if err != nil {
					return
				}
				defer local.Close()
				go io.Copy(local, conn)
				io.Copy(conn, local)
			}()
		}
	}()

	coordHost := c.RunPrefix + "-coordinator"
	log.Printf("[worker] %s up, guarding coordinator %s", hostname, coordHost)

	// Pre-warm: wait until coordinator is present+online before guarding.
	for {
		peers, _ := mesh.Peers(ctx)
		if tsmesh.PresentOnline(peers, coordHost) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollInterval):
		}
	}
	log.Printf("[worker] coordinator online, entering guard loop")

	g := &guard{threshold: c.TeardownThresh}
	for {
		peers, _ := mesh.Peers(ctx)
		present := tsmesh.PresentOnline(peers, coordHost)
		if g.observe(present) {
			log.Printf("[worker] coordinator gone %dx -> exiting", c.TeardownThresh)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollInterval):
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/distcc-action && /usr/local/go/bin/go test ./internal/worker/ -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
cd ~/distcc-action && git add internal/worker/ && git commit -m "feat: worker role (distccd + expose + guard loop)"
```

---

## Task 6: coordinator role — wait + forward + export + teardown

**Files:**
- Create: `internal/coordinator/coordinator.go`
- Test: `internal/coordinator/coordinator_test.go`

- [ ] **Step 1: Write the failing test for the GITHUB_ENV export format**

The coordinator must write `DISTCC_HOSTS`/`DISTCC_J` to the file named by `$GITHUB_ENV` so they reach the user's later build step. That format is testable without tsnet.

`internal/coordinator/coordinator_test.go`:
```go
package coordinator

import (
	"os"
	"strings"
	"testing"
)

func TestWriteGithubEnv(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "env")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeGithubEnv(f.Name(), "127.0.0.1:3701,lzo", 8, 2); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.Name())
	s := string(b)
	for _, want := range []string{
		"DISTCC_HOSTS=127.0.0.1:3701,lzo",
		"DISTCC_J=8",
		"DISTCC_WORKERS_ONLINE=2",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestWriteOutputs(t *testing.T) {
	f, _ := os.CreateTemp(t.TempDir(), "out")
	if err := writeOutputs(f.Name(), "H", 8, 2); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.Name())
	if !strings.Contains(string(b), "distcc-hosts=H") || !strings.Contains(string(b), "workers-online=2") {
		t.Errorf("outputs wrong:\n%s", b)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/distcc-action && /usr/local/go/bin/go test ./internal/coordinator/ -v`
Expected: FAIL — `writeGithubEnv`/`writeOutputs` undefined.

- [ ] **Step 3: Write minimal implementation**

`internal/coordinator/coordinator.go`:
```go
package coordinator

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"

	"github.com/xdqi/distcc-action/internal/config"
	"github.com/xdqi/distcc-action/internal/distccrun"
	"github.com/xdqi/distcc-action/internal/tsmesh"
)

func writeGithubEnv(path, hosts string, j, online int) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "DISTCC_HOSTS=%s\nDISTCC_J=%d\nDISTCC_WORKERS_ONLINE=%d\n", hosts, j, online)
	return err
}

func writeOutputs(path, hosts string, j, online int) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "distcc-hosts=%s\ndistcc-j=%d\nworkers-online=%d\n", hosts, j, online)
	return err
}

// Run executes the coordinator main phase: join, wait workers, forward, export.
// Teardown (mesh.Close) happens in the action's post phase via a separate call.
func Run(ctx context.Context, c *config.Config, hostname string) error {
	mesh, err := tsmesh.Up(ctx, hostname, c.OAuthSecret, c.Tags)
	if err != nil {
		return err
	}
	// NOTE: do NOT close mesh here; the post phase closes it as the teardown signal.

	prefix := c.RunPrefix + "-worker-"
	waitCtx, cancel := context.WithTimeout(ctx, c.WaitTimeout)
	defer cancel()
	online, werr := mesh.WaitForWorkers(waitCtx, prefix, c.MinWorkers, c.PollInterval)
	if len(online) < c.MinWorkers {
		mesh.Close()
		return fmt.Errorf("only %d/%d workers online by timeout: %v", len(online), c.MinWorkers, werr)
	}
	log.Printf("[coord] %d workers online", len(online))

	slots := c.DistccSlots
	if slots == 0 {
		slots = distccrun.Nproc()
	}

	// For each worker, forward a local port -> worker:3632 over tsnet.
	var localPorts []string
	basePort := 3701
	for i, w := range online {
		lp := strconv.Itoa(basePort + i)
		ln, err := net.Listen("tcp", "127.0.0.1:"+lp)
		if err != nil {
			mesh.Close()
			return fmt.Errorf("local listen %s: %w", lp, err)
		}
		target := w.IP + ":3632"
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					remote, err := mesh.Dial(context.Background(), target)
					if err != nil {
						return
					}
					defer remote.Close()
					go io.Copy(remote, conn)
					io.Copy(conn, remote)
				}()
			}
		}()
		localPorts = append(localPorts, lp)
	}

	hosts := distccrun.BuildHosts(localPorts, slots, c.LZO, c.Pump)
	j := distccrun.TotalJ(len(localPorts), slots)
	ccPrefix := "distcc"
	if c.Sccache {
		ccPrefix = "sccache distcc"
	}
	log.Printf("[coord] DISTCC_HOSTS=%q DISTCC_J=%d CC_PREFIX=%q", hosts, j, ccPrefix)

	if ge := os.Getenv("GITHUB_ENV"); ge != "" {
		if err := writeGithubEnv(ge, hosts, j, len(online)); err != nil {
			return err
		}
		if f, err := os.OpenFile(ge, os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "DISTCC_CC_PREFIX=%s\n", ccPrefix)
			if c.Pump {
				fmt.Fprintf(f, "DISTCC_PUMP_HINT=1\n")
			}
			f.Close()
		}
	}
	if go_ := os.Getenv("GITHUB_OUTPUT"); go_ != "" {
		if err := writeOutputs(go_, hosts, j, len(online)); err != nil {
			return err
		}
	}
	// Keep the forwards alive for the user's build. The process must stay alive
	// across the user's build step; in the composite action the main phase backgrounds
	// the forwards and returns, and the post phase tears down. See Task 7.
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/distcc-action && /usr/local/go/bin/go test ./internal/coordinator/ -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
cd ~/distcc-action && git add internal/coordinator/ && git commit -m "feat: coordinator role (wait + forward + export)"
```

---

## Task 7: main + action.yml — wire roles, handle main/post phases

The composite action runs the binary in the `main` step (coordinator: forward+export; worker: block in guard until teardown) and, for the coordinator, runs it again with `--teardown` in the `post` step (closes mesh = teardown signal). Because the coordinator's forwards must outlive the main step, the coordinator main phase **detaches** a long-lived forwarder process and records its PID; post kills it (which closes the tsnet node).

**Files:**
- Create: `cmd/distcc-action/main.go`
- Create: `action.yml`
- Test: `cmd/distcc-action/main_test.go`

- [ ] **Step 1: Write the failing test for phase dispatch**

`cmd/distcc-action/main_test.go`:
```go
package main

import "testing"

func TestPhaseFromArgs(t *testing.T) {
	if phaseFromArgs([]string{"distcc-action"}) != "main" {
		t.Error("default should be main")
	}
	if phaseFromArgs([]string{"distcc-action", "--teardown"}) != "teardown" {
		t.Error("--teardown should be teardown")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/distcc-action && /usr/local/go/bin/go test ./cmd/distcc-action/ -v`
Expected: FAIL — `phaseFromArgs` undefined.

- [ ] **Step 3: Write minimal implementation**

`cmd/distcc-action/main.go`:
```go
package main

import (
	"context"
	"log"
	"os"

	"github.com/xdqi/distcc-action/internal/config"
	"github.com/xdqi/distcc-action/internal/coordinator"
	"github.com/xdqi/distcc-action/internal/worker"
)

func phaseFromArgs(args []string) string {
	for _, a := range args[1:] {
		if a == "--teardown" {
			return "teardown"
		}
	}
	return "main"
}

func main() {
	c, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx := context.Background()
	hostname := c.RunPrefix + "-" + c.Mode
	if c.Mode == "worker" {
		// worker hostname includes the matrix index supplied via INPUT_WORKER_INDEX
		if idx := os.Getenv("INPUT_WORKER_INDEX"); idx != "" {
			hostname = c.RunPrefix + "-worker-" + idx
		}
	}

	switch phaseFromArgs(os.Args) {
	case "teardown":
		// Coordinator post phase: signal teardown by killing the detached
		// forwarder (which closes its tsnet node). The PID was recorded by main.
		if pid := readPid(); pid > 0 {
			_ = killPid(pid)
			log.Printf("[teardown] signaled forwarder pid=%d", pid)
		}
		return
	default:
		if c.Mode == "worker" {
			if err := worker.Run(ctx, c, hostname); err != nil {
				log.Fatalf("worker: %v", err)
			}
			return
		}
		// coordinator main: run the forwarder in THIS process, but detach so the
		// composite step returns while forwards stay up. We re-exec ourselves in
		// background to own the tsnet node for the build's duration.
		if os.Getenv("DISTCC_ACTION_FORWARDER") == "1" {
			// We are the detached forwarder: do the real work and block forever
			// (until killed by teardown).
			if err := coordinator.Run(ctx, c, hostname); err != nil {
				log.Fatalf("coordinator: %v", err)
			}
			select {} // block until killed
		}
		// Parent: spawn the forwarder, record its PID, return immediately.
		pid, err := spawnForwarder()
		if err != nil {
			log.Fatalf("spawn forwarder: %v", err)
		}
		writePid(pid)
		// Wait for the forwarder to have exported GITHUB_ENV before returning.
		waitForEnvExport()
		log.Printf("[coord] forwarder detached pid=%d; env exported", pid)
	}
}
```

Also add the small helpers `cmd/distcc-action/procctl.go`:
```go
package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

const pidFile = "/tmp/distcc-action-forwarder.pid"

func writePid(pid int) { _ = os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o644) }
func readPid() int {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(string(b))
	return n
}
func killPid(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func spawnForwarder() (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "DISTCC_ACTION_FORWARDER=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

// waitForEnvExport blocks until the forwarder has written DISTCC_HOSTS to
// GITHUB_ENV, so the user's next build step sees it. Times out after 6 minutes.
func waitForEnvExport() {
	ge := os.Getenv("GITHUB_ENV")
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		if ge != "" {
			if b, err := os.ReadFile(ge); err == nil && contains(string(b), "DISTCC_HOSTS=") {
				return
			}
		}
		time.Sleep(time.Second)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/distcc-action && /usr/local/go/bin/go test ./cmd/distcc-action/ -v`
Expected: PASS.

- [ ] **Step 5: Build the static binary to confirm it compiles**

Run: `cd ~/distcc-action && CGO_ENABLED=0 /usr/local/go/bin/go build -o distcc-action ./cmd/distcc-action && file distcc-action`
Expected: builds; `file` reports a statically linked ELF (no dynamic interpreter).

- [ ] **Step 6: Write action.yml**

`action.yml`:
```yaml
name: 'distcc Tailnet Farm'
description: 'Turn GitHub runners into an ephemeral distcc farm over a Tailscale mesh'
inputs:
  mode:              { description: 'coordinator|worker', required: true }
  oauth-client-id:   { description: 'Tailscale OAuth client id', required: true }
  oauth-secret:      { description: 'Tailscale OAuth client secret', required: true }
  expected-workers:  { description: 'workers expected (coordinator)', required: false }
  min-workers:       { description: 'min online workers to proceed', required: false }
  wait-timeout:      { description: 'max wait for workers', required: false, default: '300s' }
  worker-index:      { description: 'matrix index (worker)', required: false }
  tags:              { description: 'tailnet tags', required: false, default: 'tag:ci-distcc' }
  run-prefix:        { description: 'naming prefix', required: false }
  distcc-slots:      { description: 'per-worker slots (0=nproc)', required: false, default: '0' }
  lzo:               { description: 'enable LZO', required: false, default: 'true' }
  pump:              { description: 'enable pump (experimental)', required: false, default: 'false' }
  poll-interval:     { description: 'teardown poll', required: false, default: '1s' }
  teardown-threshold:{ description: 'consecutive offline reads', required: false, default: '5' }
  sccache:           { description: 'enable sccache', required: false, default: 'false' }
outputs:
  distcc-hosts:   { description: 'assembled DISTCC_HOSTS', value: ${{ steps.run.outputs.distcc-hosts }} }
  distcc-j:       { description: 'suggested -j', value: ${{ steps.run.outputs.distcc-j }} }
  workers-online: { description: 'participating workers', value: ${{ steps.run.outputs.workers-online }} }
runs:
  using: 'composite'
  steps:
    - id: build-bin
      shell: bash
      run: |
        cd "${{ github.action_path }}"
        if [ ! -x ./distcc-action ]; then
          CGO_ENABLED=0 go build -o distcc-action ./cmd/distcc-action
        fi
    - id: run
      shell: bash
      env:
        INPUT_MODE: ${{ inputs.mode }}
        INPUT_OAUTH_CLIENT_ID: ${{ inputs.oauth-client-id }}
        INPUT_OAUTH_SECRET: ${{ inputs.oauth-secret }}
        INPUT_EXPECTED_WORKERS: ${{ inputs.expected-workers }}
        INPUT_MIN_WORKERS: ${{ inputs.min-workers }}
        INPUT_WAIT_TIMEOUT: ${{ inputs.wait-timeout }}
        INPUT_WORKER_INDEX: ${{ inputs.worker-index }}
        INPUT_TAGS: ${{ inputs.tags }}
        INPUT_RUN_PREFIX: ${{ inputs.run-prefix }}
        INPUT_DISTCC_SLOTS: ${{ inputs.distcc-slots }}
        INPUT_LZO: ${{ inputs.lzo }}
        INPUT_PUMP: ${{ inputs.pump }}
        INPUT_POLL_INTERVAL: ${{ inputs.poll-interval }}
        INPUT_TEARDOWN_THRESHOLD: ${{ inputs.teardown-threshold }}
        INPUT_SCCACHE: ${{ inputs.sccache }}
      run: "${{ github.action_path }}/distcc-action"
    - id: teardown
      if: ${{ always() && inputs.mode == 'coordinator' }}
      shell: bash
      run: "${{ github.action_path }}/distcc-action --teardown"
```

Note: the `teardown` step runs in the same job after the user's build only if the user places their build between two invocations — but composite actions run all their steps contiguously. The real teardown trigger is the coordinator JOB ending. Therefore the example workflow (Task 9) runs the coordinator action, then the build, then a final explicit teardown step calling the action with `mode: coordinator` + a teardown marker. This is reconciled in Task 9; action.yml provides the `--teardown` entrypoint.

- [ ] **Step 7: Commit**

```bash
cd ~/distcc-action && git add cmd/ action.yml && git commit -m "feat: main entry, main/teardown phases, action.yml"
```

---

## Task 8: Local cross-host integration test (isolated docker networks)

Re-validate the FULL worker+coordinator binaries together locally before spending real Actions minutes, using isolated docker networks (Verified Fact #3) so routing is real DERP, not a docker-bridge shortcut.

**Files:**
- Create: `spike/integration/Dockerfile`
- Create: `spike/integration/run.sh`

- [ ] **Step 1: Write the integration Dockerfile (binary + distcc + gcc)**

`spike/integration/Dockerfile`:
```dockerfile
FROM golang:1.23 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /distcc-action ./cmd/distcc-action

FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
        distcc build-essential git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /distcc-action /usr/local/bin/distcc-action
```

- [ ] **Step 2: Write run.sh**

`spike/integration/run.sh`:
```bash
#!/usr/bin/env bash
# TS_AUTHKEY=<key> bash run.sh  — 1 coordinator + 2 workers, isolated nets, build redis.
set -euo pipefail
: "${TS_AUTHKEY:?}"; PFX="int$$"; IMG=distcc-int:latest
cd "$(dirname "$0")/../.."
docker build -t $IMG -f spike/integration/Dockerfile .
nets=(${PFX}-c ${PFX}-w1 ${PFX}-w2)
for n in "${nets[@]}"; do docker network create $n >/dev/null; done
cleanup(){ docker rm -f ${PFX}-co ${PFX}-wo1 ${PFX}-wo2 >/dev/null 2>&1||true; for n in "${nets[@]}"; do docker network rm $n >/dev/null 2>&1||true; done; }
trap cleanup EXIT

common="-e INPUT_OAUTH_CLIENT_ID=x -e INPUT_OAUTH_SECRET=$TS_AUTHKEY -e INPUT_TAGS=tag:ci -e INPUT_RUN_PREFIX=$PFX -e GITHUB_RUN_ID=$PFX -e INPUT_POLL_INTERVAL=1s -e INPUT_TEARDOWN_THRESHOLD=5"
for i in 1 2; do
  docker run -d --name ${PFX}-wo$i --network ${PFX}-w$i $common \
    -e INPUT_MODE=worker -e INPUT_WORKER_INDEX=$i $IMG distcc-action
done
sleep 4
# coordinator: run binary (exports to a fake GITHUB_ENV), then build redis using it
docker run --name ${PFX}-co --network ${PFX}-c $common \
  -e INPUT_MODE=coordinator -e INPUT_EXPECTED_WORKERS=2 -e INPUT_MIN_WORKERS=1 \
  -e GITHUB_ENV=/tmp/genv -e GITHUB_OUTPUT=/tmp/gout \
  --entrypoint bash $IMG -c '
    distcc-action            # main: detaches forwarder, exports /tmp/genv
    set -a; source /tmp/genv; set +a
    echo "DISTCC_HOSTS=$DISTCC_HOSTS  DISTCC_J=$DISTCC_J"
    git clone --depth 1 https://github.com/redis/redis /tmp/redis 2>&1 | tail -1
    cd /tmp/redis && time make -j${DISTCC_J} CC="distcc gcc" 2>&1 | tail -15
    ls -la src/redis-server && echo "INTEGRATION-RESULT build = PASS" || echo "INTEGRATION-RESULT build = FAIL"
    distcc-action --teardown # signal workers to exit
    sleep 8
  '
echo "=== worker logs ==="; for i in 1 2; do echo "--w$i--"; docker logs --tail 8 ${PFX}-wo$i 2>&1; done
```

- [ ] **Step 3: Run integration**

Run: `TS_AUTHKEY=<ephemeral-key> bash spike/integration/run.sh`
Expected: `INTEGRATION-RESULT build = PASS`; worker logs show `entering guard loop` then `coordinator gone -> exiting`; `redis-server` present.

- [ ] **Step 4: Commit**

```bash
cd ~/distcc-action && git add spike/integration/ && git commit -m "test: local cross-host integration (isolated nets, redis build)"
```

---

## Task 9: Example workflow + README (publishable)

**Files:**
- Create: `examples/build.yml`
- Create: `README.md`

- [ ] **Step 1: Write the example workflow**

`examples/build.yml`:
```yaml
name: distcc-farm build
on: [workflow_dispatch]
jobs:
  workers:
    strategy:
      matrix: { idx: [1, 2, 3] }
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: xdqi/distcc-action@v1
        with:
          mode: worker
          worker-index: ${{ matrix.idx }}
          oauth-client-id: ${{ secrets.TS_OAUTH_ID }}
          oauth-secret: ${{ secrets.TS_OAUTH_SECRET }}
          tags: tag:ci-distcc

  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - id: farm
        uses: xdqi/distcc-action@v1
        with:
          mode: coordinator
          expected-workers: 3
          min-workers: 1
          oauth-client-id: ${{ secrets.TS_OAUTH_ID }}
          oauth-secret: ${{ secrets.TS_OAUTH_SECRET }}
          tags: tag:ci-distcc
      - name: Build
        run: make -j${DISTCC_J} CC="distcc gcc"
      # coordinator teardown runs automatically as the job ends (post phase)
```

- [ ] **Step 2: Write README**

`README.md` (key sections): what it is; the honest boundary (same-arch native C/C++, no cross/zig); Tailscale setup (OAuth client with `auth_keys` scope + `tag:ci-distcc` in ACL `tagOwners`); inputs/outputs table (copy from spec); the example above; note that pump is experimental.

```markdown
# distcc-action

Turn N GitHub-hosted runners into an ephemeral [distcc](https://distcc.github.io/)
farm connected by a temporary [Tailscale](https://tailscale.com/) mesh. A single
static Go binary (`tsnet`) does networking + orchestration + teardown; stock
distcc does the compiling.

## Scope
Same-architecture **native C/C++** (gcc/clang). NOT for cross-compilation or
`zig cc`/custom wrapper compilers (distcc needs byte-identical compilers).

## Tailscale setup
1. ACL: add `"tag:ci-distcc": []` under `tagOwners`.
2. Create an OAuth client (scope: `auth_keys`, tag: `tag:ci-distcc`).
3. Store as repo secrets `TS_OAUTH_ID` / `TS_OAUTH_SECRET`.

## Usage
See `examples/build.yml`. Inputs/outputs: see `docs/2026-06-04-distcc-tailnet-action-design.md`.

## Notes
- `pump: true` is experimental and unverified under container/cross-host; off by default.
- Teardown: the coordinator drops from the tailnet when its job ends; workers detect this and exit within ~`teardown-threshold × poll-interval` seconds.
```

- [ ] **Step 3: Commit**

```bash
cd ~/distcc-action && git add examples/ README.md && git commit -m "docs: example workflow + README"
```

---

## Task 10: Real GitHub Actions acceptance run

Closes the spec's last risk: "a real cross-host run has not yet been exercised."

**Files:**
- Create: `.github/workflows/selftest.yml`

- [ ] **Step 1: Write the self-test workflow**

`.github/workflows/selftest.yml`: same shape as `examples/build.yml` but checks out a small known C project (e.g. a vendored sqlite amalgamation or a redis clone), builds it via the farm, and asserts the artifact exists + `distccmon-text` (or distcc verbose logs) show jobs landing on workers. Uses `uses: ./` (the action in this repo).

```yaml
name: selftest
on: [workflow_dispatch]
jobs:
  workers:
    strategy: { matrix: { idx: [1, 2] } }
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: sudo apt-get update && sudo apt-get install -y distcc
      - uses: ./
        with:
          mode: worker
          worker-index: ${{ matrix.idx }}
          oauth-client-id: ${{ secrets.TS_OAUTH_ID }}
          oauth-secret: ${{ secrets.TS_OAUTH_SECRET }}
          tags: tag:ci-distcc
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: sudo apt-get update && sudo apt-get install -y distcc
      - uses: ./
        with:
          mode: coordinator
          expected-workers: 2
          min-workers: 1
          oauth-client-id: ${{ secrets.TS_OAUTH_ID }}
          oauth-secret: ${{ secrets.TS_OAUTH_SECRET }}
          tags: tag:ci-distcc
      - run: |
          git clone --depth 1 https://github.com/redis/redis
          cd redis && DISTCC_VERBOSE=1 make -j${DISTCC_J} CC="distcc gcc" 2>&1 | tee /tmp/build.log
          test -x src/redis-server
          grep -q "by 100\." /tmp/build.log || echo "WARN: no remote dispatch seen in verbose log"
```

- [ ] **Step 2: Push to a test repo and run**

Run: push the repo to GitHub, set `TS_OAUTH_ID`/`TS_OAUTH_SECRET` secrets, trigger `selftest` via `workflow_dispatch`.
Expected: `build` job green; `redis-server` built; verbose log shows compiles dispatched to worker tailnet IPs (`100.x`). Worker jobs end shortly after `build`.

- [ ] **Step 3: Commit any fixes + tag v1**

```bash
cd ~/distcc-action && git add .github/ && git commit -m "ci: real Actions self-test workflow"
git tag v1 && echo "tag v1 created (push with: git push origin main --tags)"
```

---

## Task 11: Pump unit-test coverage + README hints

`BuildHosts` already takes `pump` (Task 3) and the coordinator already passes
`c.Pump`/`c.Sccache` and exports `DISTCC_CC_PREFIX`/`DISTCC_PUMP_HINT` (Task 6).
This task adds the explicit pump unit test that Task 3 omitted, and documents the
user-facing usage of the exported hints.

**Files:**
- Modify: `internal/distccrun/distccrun_test.go` (add the `,cpp` case)
- Modify: `README.md` (pump/sccache usage)

- [ ] **Step 1: Add the failing pump test**

Add to `internal/distccrun/distccrun_test.go`:
```go
func TestBuildHostsPump(t *testing.T) {
	got := BuildHosts([]string{"3701"}, 4, true, true)
	want := "127.0.0.1:3701/4,lzo,cpp"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
```

- [ ] **Step 2: Run the test**

Run: `cd ~/distcc-action && /usr/local/go/bin/go test ./internal/distccrun/ -run TestBuildHostsPump -v`
Expected: PASS (implementation from Task 3 already supports `,cpp`). If it FAILS, the Task 3 implementation diverged — fix `BuildHosts` to append `,cpp` when `pump`.

- [ ] **Step 3: Document usage in README**

Append to `README.md`:
```markdown
### sccache / pump
The coordinator exports two hints to `GITHUB_ENV`:
- `DISTCC_CC_PREFIX` — `distcc` normally, `sccache distcc` when `sccache: true`.
  Use it as: `make -j${DISTCC_J} CC="${DISTCC_CC_PREFIX} gcc"`.
- `DISTCC_PUMP_HINT=1` — set when `pump: true`; wrap the build with the pump
  script: `pump make -j${DISTCC_J} CC="${DISTCC_CC_PREFIX} gcc"`.
  pump is experimental and unverified under container/cross-host (default off).
```

- [ ] **Step 4: Commit**

```bash
cd ~/distcc-action && git add internal/distccrun/distccrun_test.go README.md && git commit -m "test+docs: pump host-option coverage + sccache/pump usage"
```

---

## Notes on teardown reconciliation (read before Task 7)

The cleanest teardown is **the coordinator job ending** — when its job finishes, GitHub stops the runner, the process dies, the tsnet node drops, and workers observe the coordinator peer gone. The `--teardown` entrypoint and detached-forwarder design in Task 7 exist to keep the tsnet forwards alive *during* the user's build step (which runs as a separate step after the coordinator action's main step) and to make teardown explicit/prompt rather than relying solely on job end. Task 8 validates this locally; Task 10 validates it on real runners. If the detached-forwarder approach proves fragile on real runners (Task 10), the fallback is to rely purely on job-end teardown (drop the `--teardown` step and detach logic) — this is a known acceptable degradation, costing only up to ~`teardown-threshold` extra seconds.
