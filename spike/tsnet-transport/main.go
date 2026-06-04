// Spike: prove a tsnet node can forward a local TCP port to a remote distccd
// over the tailnet, and that distcc compiles a .c through that forward.
//
// role=server : tsnet up, accept tailnet TCP on :3632, proxy to 127.0.0.1:3632
//
//	(a plain distccd running in the same container)
//
// role=client : tsnet up, listen on 127.0.0.1:LOCAL, dial the server peer's
//
//	:3632 over tsnet, splice. Then run distcc against 127.0.0.1:LOCAL.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
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
		Hostname:      os.Getenv("TS_HOSTNAME"),
		AuthKey:       os.Getenv("TS_AUTHKEY"),
		Ephemeral:     true,
		Dir:           "/tmp/tsnet-" + os.Getenv("TS_HOSTNAME"),
		AdvertiseTags: []string{"tag:ci"},
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
		serverHost := os.Getenv("SERVER_HOST")
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
