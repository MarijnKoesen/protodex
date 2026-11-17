// SPDX-License-Identifier: Apache-2.0

// Package version exposes build information for Protodex binaries.
package version

// Version is the release version, overridden at build time via
// -ldflags "-X github.com/marijnkoesen/protodex/internal/version.Version=...".
var Version = "dev"

// String returns a human-readable version string.
func String() string {
	return "protodex " + Version
}
