// SPDX-License-Identifier: Apache-2.0

// Command fetchbuf downloads checksum-verified buf CLI binaries for the
// conformance suite, and pins checksums of new releases.
//
//	fetchbuf get [-dir DIR] VERSION...   print the path of each verified binary
//	fetchbuf pin VERSION...              add release checksums to checksums.txt
//	fetchbuf versions [-pr] [-json]      list the supported versions
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/marijnkoesen/protodex/test/conformance/bufdl"
)

const usage = `usage:
  fetchbuf get [-dir DIR] VERSION...   download, verify and print binary paths
  fetchbuf pin [-file FILE] VERSION... pin release checksums (review the diff!)
  fetchbuf versions [-pr] [-json]     print supported versions (-pr: PR subset)
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(os.Stderr, "fetchbuf:", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return errors.New("missing command")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	switch args[0] {
	case "get":
		dir := fs.String("dir", defaultDir(), "cache directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		d := &bufdl.Downloader{Dir: *dir}
		for _, v := range fs.Args() {
			p, err := d.Get(ctx, v)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(stdout, p); err != nil {
				return err
			}
		}
		return nil
	case "pin":
		file := fs.String("file", "test/conformance/bufdl/checksums.txt", "checksums file to update")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		old, err := os.ReadFile(*file) //nolint:gosec // path given by the developer
		if err != nil {
			return err
		}
		sums, err := bufdl.ParseChecksums(string(old))
		if err != nil {
			return err
		}
		d := &bufdl.Downloader{}
		for _, v := range fs.Args() {
			v = strings.TrimPrefix(v, "v")
			got, err := d.FetchReleaseChecksums(ctx, v)
			if err != nil {
				return err
			}
			sums[v] = got
		}
		return os.WriteFile(*file, []byte(header(string(old))+sums.Format()), 0o600) //nolint:gosec // path given by the developer
	case "versions":
		pr := fs.Bool("pr", false, "only the subset run on every pull request")
		asJSON := fs.Bool("json", false, "print a JSON array (for a CI matrix)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		vs, err := bufdl.Supported(*pr)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(stdout).Encode(vs)
		}
		_, err = fmt.Fprintln(stdout, strings.Join(vs, " "))
		return err
	case "help", "-h", "--help":
		_, err := fmt.Fprint(stdout, usage)
		return err
	}
	_, _ = fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

// header keeps the leading comment block of checksums.txt.
func header(s string) string {
	var b strings.Builder
	for line := range strings.SplitAfterSeq(s, "\n") {
		if !strings.HasPrefix(line, "#") {
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

func defaultDir() string {
	if d := os.Getenv("PROTODEX_CONFORMANCE_CACHE"); d != "" {
		return d
	}
	return bufdl.DefaultCacheDir()
}
