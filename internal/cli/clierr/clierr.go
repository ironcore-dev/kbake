// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package clierr provides the error type the CLI uses to distinguish runtime
// failures (a bad Kernelfile, a build error, ...) from usage failures (an
// unknown command, a bad flag) so that package cli can pick the right process
// exit code.
package clierr

// RuntimeError wraps an error that is a runtime failure rather than a usage
// error. Package cli maps it to exit code 1; usage errors (unwrapped errors
// returned by cobra, such as flag-parse or unknown-command errors) map to 2.
type RuntimeError struct {
	err error
}

// Wrap wraps err as a RuntimeError. A nil err returns nil.
func Wrap(err error) error {
	if err == nil {
		return nil
	}
	return &RuntimeError{err: err}
}

func (e *RuntimeError) Error() string { return e.err.Error() }

// Unwrap allows errors.Is/errors.As to reach the underlying cause.
func (e *RuntimeError) Unwrap() error { return e.err }
