// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if path := os.Getenv("PROTODEX_CONFORMANCE_REPORT"); path != "" {
		if err := writeReportFile(path); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "conformance report:", err)
			code = 1
		}
	}
	if pluginPath != "" {
		_ = os.RemoveAll(strings.TrimSuffix(pluginPath, "/protoc-gen-conformance"))
	}
	os.Exit(code)
}

func writeReportFile(path string) error {
	f, err := os.Create(path) //nolint:gosec // path chosen by the developer
	if err != nil {
		return err
	}
	if err := writeReport(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// TestConformance runs every scenario for every selected backend and buf
// CLI: TestConformance/<backend>/buf-<version>/<scenario>.
func TestConformance(t *testing.T) {
	fault := os.Getenv("PROTODEX_CONFORMANCE_FAULT")
	for _, b := range selectedBackends() {
		t.Run(b.Name, func(t *testing.T) {
			if b.Skip != nil {
				if reason := b.Skip(); reason != "" {
					t.Skip(reason)
				}
			}
			for _, cli := range bufCLIs(t) {
				t.Run("buf-"+cli.Version, func(t *testing.T) {
					for _, s := range scenarios {
						t.Run(s.Name, func(t *testing.T) {
							t.Parallel()
							outcome := "pass"
							t.Cleanup(func() {
								switch {
								case t.Failed():
									outcome = "**FAIL**"
								case t.Skipped():
									outcome = "n/a"
								}
								record(b.Name, cli.Version, s.Name, outcome)
							})
							if s.Needs != nil {
								if reason := s.Needs(cli); reason != "" {
									t.Skipf("buf %s: %s", cli.Version, reason)
								}
							}
							s.Run(t, newEnv(t, cli, b, fault))
						})
					}
				})
			}
		})
	}
}

// TestDigestFaultIsCaught proves the suite catches a server that hands out
// wrong commit digests: with the fault injected, the digest-sensitive
// scenarios must fail. It uses the newest available buf CLI and the first
// backend.
func TestDigestFaultIsCaught(t *testing.T) {
	all := bufCLIs(t)
	cli := all[len(all)-1]
	for _, c := range all {
		if !c.atLeast(cli.Version) {
			continue
		}
		cli = c
	}
	b := backends[0]
	for _, name := range []string{"dep-update", "build"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := scenarioByName(t, name)
			rec := runRecorded(t, func(r T) { s.Run(r, newEnv(r, cli, b, FaultDigest)) })
			if !rec.failed {
				t.Fatalf("scenario %s passed with digest fault injected (buf %s); the suite would not catch a digest bug", name, cli.Version)
			}
			if !strings.Contains(strings.ToLower(rec.msg), "digest") {
				t.Fatalf("scenario %s failed for another reason than the digest (buf %s):\n%s", name, cli.Version, rec.msg)
			}
			t.Logf("caught (buf %s): %s", cli.Version, rec.msg)
		})
	}
}

func scenarioByName(t *testing.T, name string) Scenario {
	t.Helper()
	for _, s := range scenarios {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no scenario %q", name)
	return Scenario{}
}

// recorder is a T that records a failure instead of failing the test, so a
// scenario can be run where failure is the expected outcome.
type recorder struct {
	*testing.T
	mu     sync.Mutex
	failed bool
	msg    string
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.mu.Lock()
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
	r.mu.Unlock()
	runtime.Goexit()
}

func (r *recorder) Skipf(format string, args ...any) {
	r.Fatalf("unexpected skip: "+format, args...)
}

func (r *recorder) Failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}

// runRecorded runs fn on a recorder in its own goroutine (so Fatalf can
// Goexit) and returns the recorder once fn has finished.
func runRecorded(t *testing.T, fn func(T)) *recorder {
	r := &recorder{T: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(r)
	}()
	<-done
	return r
}
