package distccrun

import (
	"fmt"
	"os"
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
// distccd logs to stderr at logLevel; we wire its stdout/stderr to ours so the
// daemon's per-job log lines stream live into the worker's CI job log.
func StartDaemon(port, jobs int, logLevel string) (*exec.Cmd, error) {
	if logLevel == "" {
		logLevel = "info"
	}
	cmd := exec.Command("distccd",
		"--daemon", "--no-detach",
		"--allow", "0.0.0.0/0", // tailnet-only reachability is enforced by the tsnet listener
		"--listen", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--jobs", strconv.Itoa(jobs),
		"--log-stderr", "--log-level", logLevel,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start distccd: %w", err)
	}
	return cmd, nil
}
