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
	dd, err := distccrun.StartDaemon(3632, slots, c.DistccLogLevel)
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
