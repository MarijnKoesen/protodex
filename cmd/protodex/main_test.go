// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marijnkoesen/protodex/internal/store/sqlite"
)

func TestMigrateCommand(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	for _, dir := range []string{"up", "up", "down"} {
		var out, errOut bytes.Buffer
		if err := run(context.Background(), []string{"migrate", "-db", db, dir}, &out, &errOut); err != nil {
			t.Fatalf("migrate %s: %v", dir, err)
		}
		if !strings.Contains(out.String(), "ok") {
			t.Fatalf("output = %q", out.String())
		}
	}
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"migrate", "-db", db, "sideways"}, &out, &errOut); err == nil {
		t.Fatal("want error for bad direction")
	}
	if err := run(context.Background(), []string{"migrate"}, &out, &errOut); err == nil {
		t.Fatal("want error without -db")
	}
}

func TestServeRefusedWhenDBLocked(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var out, errOut bytes.Buffer
	err = run(context.Background(), []string{"serve", "-addr", "127.0.0.1:0", "-db-path", db}, &out, &errOut)
	if !errors.Is(err, sqlite.ErrLocked) {
		t.Fatalf("serve error = %v, want ErrLocked", err)
	}
}

func TestBootstrapCommand(t *testing.T) {
	db := filepath.Join(t.TempDir(), "b.db")
	var out, errb bytes.Buffer
	if err := bootstrap(context.Background(), []string{"-db", db}, func(string) string { return "" }, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "pdx_") {
		t.Fatalf("token not printed: %q", out.String())
	}
	out.Reset()
	if err := bootstrap(context.Background(), []string{"-db", db}, func(string) string { return "" }, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "pdx_") || !strings.Contains(out.String(), "already exists") {
		t.Fatalf("second run: %q", out.String())
	}
}
