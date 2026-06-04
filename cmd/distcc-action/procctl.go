package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
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
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func killPid(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func spawnForwarder() (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	// Filter out RUNNER_TRACKING_ID so the GitHub runner does not kill this
	// detached process at step boundaries; it must live until job end.
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "RUNNER_TRACKING_ID=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "DISTCC_ACTION_FORWARDER=1")
	cmd := exec.Command(exe)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // own session, detach from step process group
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

// waitForEnvExport blocks until the forwarder has written DISTCC_HOSTS to
// GITHUB_ENV, so the user's next build step sees it. Times out after 6 minutes.
// Returns true if DISTCC_HOSTS was observed, false on timeout.
func waitForEnvExport() bool {
	ge := os.Getenv("GITHUB_ENV")
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		if ge != "" {
			if b, err := os.ReadFile(ge); err == nil && strings.Contains(string(b), "DISTCC_HOSTS=") {
				return true
			}
		}
		time.Sleep(time.Second)
	}
	return false
}
