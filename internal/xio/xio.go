// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package xio

type WriterFunc func(p []byte) (n int, err error)

func (f WriterFunc) Write(p []byte) (n int, err error) {
	return f(p)
}
