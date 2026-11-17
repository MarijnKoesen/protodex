// SPDX-License-Identifier: Apache-2.0

// Command capture runs the traffic-capture proxy or builds the RPC matrix.
//
//	capture proxy  -upstream https://buf.build -listen :8443 -out f.jsonl -cli-version 1.50.0 -scenario push
//	capture matrix -dir test/capture/out
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"example.com/openbuf/test/capture"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: capture proxy|matrix [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "proxy":
		runProxy(os.Args[2:])
	case "matrix":
		runMatrix(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand", os.Args[1])
		os.Exit(2)
	}
}

func runProxy(args []string) {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	upstream := fs.String("upstream", "https://buf.build", "upstream registry URL")
	listen := fs.String("listen", "127.0.0.1:8443", "listen address")
	out := fs.String("out", "capture.jsonl", "JSONL output file (appended)")
	ver := fs.String("cli-version", "unknown", "buf CLI version label")
	scen := fs.String("scenario", "unknown", "scenario label")
	cert := fs.String("tls-cert", "", "TLS certificate (PEM); empty = plain HTTP")
	key := fs.String("tls-key", "", "TLS key (PEM)")
	dial := fs.String("upstream-addr", "", "dial this host:port instead of resolving the upstream name (avoids loops when /etc/hosts points the registry at the proxy)")
	keep := fs.Bool("keep-binary", false, "also store non-JSON bodies (may contain credentials; review before sharing)")
	_ = fs.Parse(args)

	u, err := url.Parse(*upstream)
	if err != nil {
		log.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Clean(*out), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	p := &capture.Proxy{Upstream: u, Out: f, CLIVersion: *ver, Scenario: *scen, KeepBinary: *keep}
	if *dial != "" {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, *dial)
		}
		p.Transport = tr
	}
	srv := &http.Server{Addr: *listen, Handler: p.Handler(), ReadHeaderTimeout: 10 * time.Second}
	if *cert != "" {
		log.Fatal(srv.ListenAndServeTLS(*cert, *key))
	}
	log.Fatal(srv.ListenAndServe())
}

func runMatrix(args []string) {
	fs := flag.NewFlagSet("matrix", flag.ExitOnError)
	dir := fs.String("dir", "test/capture/out", "directory containing *.jsonl recordings")
	_ = fs.Parse(args)
	files, _ := filepath.Glob(filepath.Join(*dir, "*", "*.jsonl"))
	m := capture.NewMatrix()
	for _, name := range files {
		f, err := os.Open(filepath.Clean(name))
		if err != nil {
			log.Fatal(err)
		}
		err = m.Add(f)
		_ = f.Close()
		if err != nil {
			log.Fatalf("%s: %v", name, err)
		}
	}
	fmt.Print(m.Markdown())
}
