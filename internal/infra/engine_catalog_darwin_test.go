//go:build darwin

package infra

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestEngineCatalogReaderRejectsFIFOBeforeOpening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := openEngineCatalogFile
	opened := false
	openEngineCatalogFile = func(string) (*os.File, error) {
		opened = true
		return nil, os.ErrPermission
	}
	t.Cleanup(func() { openEngineCatalogFile = previous })

	_, state, err := ReadEngineCatalogEntry(path, "qwen-local")
	if opened || state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "catalog.file") {
		t.Fatalf("FIFO catalog was not rejected before open: opened=%v state=%s err=%v", opened, state, err)
	}
}
