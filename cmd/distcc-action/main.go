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
	// Teardown needs no config: handle it before config.Load so that an explicit
	// --teardown invocation (e.g. from a user-supplied workflow step) works even
	// when INPUT_MODE is unset.
	if phaseFromArgs(os.Args) == "teardown" {
		if pid := readPid(); pid > 0 {
			_ = killPid(pid)
			log.Printf("[teardown] signaled forwarder pid=%d", pid)
		}
		return
	}

	c, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx := context.Background()
	hostname := c.RunPrefix + "-" + c.Mode
	if c.Mode == "worker" {
		if idx := os.Getenv("INPUT_WORKER_INDEX"); idx != "" {
			hostname = c.RunPrefix + "-worker-" + idx
		}
	}

	if c.Mode == "worker" {
		if err := worker.Run(ctx, c, hostname); err != nil {
			log.Fatalf("worker: %v", err)
		}
		return
	}
	// coordinator main: the real work runs in a DETACHED copy of ourselves so
	// the composite step can return while the tsnet forwards stay up for the
	// user's build. The detached copy sets DISTCC_ACTION_FORWARDER=1.
	if os.Getenv("DISTCC_ACTION_FORWARDER") == "1" {
		if err := coordinator.Run(ctx, c, hostname); err != nil {
			log.Fatalf("coordinator: %v", err)
		}
		select {} // block until job end (runner tears down the detached process)
	}
	pid, err := spawnForwarder()
	if err != nil {
		log.Fatalf("spawn forwarder: %v", err)
	}
	writePid(pid)
	if !waitForEnvExport() {
		log.Fatalf("coordinator forwarder did not export DISTCC_HOSTS within timeout")
	}
	log.Printf("[coord] forwarder detached pid=%d; env exported", pid)
}
