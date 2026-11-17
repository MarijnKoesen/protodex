// SPDX-License-Identifier: Apache-2.0

// Command protodex is the Protodex server. It is currently a placeholder.
package main

import (
	"fmt"
	"os"

	"github.com/marijnkoesen/protodex/internal/version"
)

func main() {
	if _, err := fmt.Fprintln(os.Stdout, version.String()); err != nil {
		os.Exit(1)
	}
}
