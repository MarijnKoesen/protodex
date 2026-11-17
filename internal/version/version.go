// SPDX-License-Identifier: Apache-2.0

// Package version exposes build information for Openbuf binaries.
package version

// Version is the release version, overridden at build time via
// -ldflags "-X example.com/openbuf/internal/version.Version=...".
var Version = "dev"

// String returns a human-readable version string.
func String() string {
	return "openbuf " + Version
}
