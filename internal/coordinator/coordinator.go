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
	// Keep forwards alive for the user's build; this function returns here and
	// the caller (detached forwarder process) blocks on select{} until job end,
	// at which point the runner tears it down.
	return nil
}
