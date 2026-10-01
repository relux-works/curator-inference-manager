//go:build windows

package main

import "io"

func runLegacyImport(_ []string, _ io.Writer, stderr io.Writer) int {
	_ = writeLine(stderr, "legacy import unsupported on Windows")
	return 78
}
func runLegacyEvidence(_ []string, _ io.Writer, stderr io.Writer) int {
	_ = writeLine(stderr, "legacy evidence unsupported on Windows")
	return 78
}
func legacyImportExitCode(error) (int, bool) { return 0, false }
