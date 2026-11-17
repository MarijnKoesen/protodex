// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Matrix aggregates recordings into RPC x CLI version.
type Matrix struct {
	// cells[rpc][version] = scenarios in which it was seen
	cells    map[string]map[string]map[string]bool
	versions map[string]bool
}

// NewMatrix returns an empty Matrix.
func NewMatrix() *Matrix {
	return &Matrix{cells: map[string]map[string]map[string]bool{}, versions: map[string]bool{}}
}

// Add reads JSONL exchanges from r.
func (m *Matrix) Add(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		var e Exchange
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return err
		}
		name := e.Service + "/" + e.RPC
		if e.Service == "" {
			name = e.Method + " " + e.Path
		}
		m.versions[e.CLIVersion] = true
		if m.cells[name] == nil {
			m.cells[name] = map[string]map[string]bool{}
		}
		if m.cells[name][e.CLIVersion] == nil {
			m.cells[name][e.CLIVersion] = map[string]bool{}
		}
		m.cells[name][e.CLIVersion][e.Scenario] = true
	}
	return sc.Err()
}

// Markdown renders the matrix. Cells list the scenarios that called the RPC.
func (m *Matrix) Markdown() string {
	vs := make([]string, 0, len(m.versions))
	for v := range m.versions {
		vs = append(vs, v)
	}
	sort.Slice(vs, func(i, j int) bool { return versionLess(vs[i], vs[j]) })
	rpcs := make([]string, 0, len(m.cells))
	for k := range m.cells {
		rpcs = append(rpcs, k)
	}
	sort.Strings(rpcs)

	var b strings.Builder
	b.WriteString("| RPC |")
	for _, v := range vs {
		b.WriteString(" " + v + " |")
	}
	b.WriteString("\n|---|")
	for range vs {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, rpc := range rpcs {
		b.WriteString("| `" + rpc + "` |")
		for _, v := range vs {
			sc := m.cells[rpc][v]
			if len(sc) == 0 {
				b.WriteString(" – |")
				continue
			}
			names := make([]string, 0, len(sc))
			for s := range sc {
				names = append(names, s)
			}
			sort.Strings(names)
			b.WriteString(" " + strings.Join(names, ", ") + " |")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func versionLess(a, b string) bool {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, ex := strconv.Atoi(pa[i])
		y, ey := strconv.Atoi(pb[i])
		if ex != nil || ey != nil {
			return a < b
		}
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}
