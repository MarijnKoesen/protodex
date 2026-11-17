// SPDX-License-Identifier: Apache-2.0

// Package module holds the module and label naming rules shared by the
// ModuleService and LabelService handlers.
package module

import (
	"errors"
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// DefaultLabel is the default label name of a new module when the request
// does not name one.
const DefaultLabel = "main"

// Name length limits, from the registry API's validation rules.
const (
	MinNameLen      = 2
	MaxNameLen      = 100
	MaxLabelNameLen = 250
)

var namePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// ErrInvalidName means a module or label name breaks the naming rules.
var ErrInvalidName = errors.New("invalid name")

// ValidateName checks a module name: 2 to 100 lowercase letters, digits and
// inner hyphens, starting with a letter (the owner naming rules, with the
// registry API's module length limits).
func ValidateName(name string) error {
	if len(name) < MinNameLen || len(name) > MaxNameLen {
		return fmt.Errorf("%w: module name %q must be %d to %d characters", ErrInvalidName, name, MinNameLen, MaxNameLen)
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w: module name %q: use lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen", ErrInvalidName, name)
	}
	return nil
}

// ValidateLabelName checks a label name: 1 to 250 characters of UTF-8 without
// whitespace or control characters. Labels commonly mirror VCS branch and
// tag names, so dots, slashes and similar punctuation are allowed.
func ValidateLabelName(name string) error {
	if name == "" || utf8.RuneCountInString(name) > MaxLabelNameLen {
		return fmt.Errorf("%w: label name %q must be 1 to %d characters", ErrInvalidName, name, MaxLabelNameLen)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("%w: label name must be valid UTF-8", ErrInvalidName)
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("%w: label name %q must not contain whitespace or control characters", ErrInvalidName, name)
		}
	}
	return nil
}
