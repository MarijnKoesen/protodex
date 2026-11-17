// SPDX-License-Identifier: Apache-2.0

// Command openbuf is the Openbuf server. It is currently a placeholder.
package main

import (
	"fmt"
	"os"

	"github.com/MarijnKoesen/openbuf/internal/version"
)

func main() {
	if _, err := fmt.Fprintln(os.Stdout, version.String()); err != nil {
		os.Exit(1)
	}
}
