//go:build !windows

package infra

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const sharedRuntimeSocketPathLimit = 103

func sharedRuntimeSocketPath(root, key string) string {
	return filepath.Join(root, sharedRuntimeStateNamespace, sharedRuntimeRendezvousName, key[:32], "b.sock")
}

// ValidateEngineCacheRoot checks the canonical socket budget without creating state.
// Missing descendants are resolved through their nearest existing ancestor.
func ValidateEngineCacheRoot(root string) error {
	if root == "" {
		var err error
		root, err = os.UserCacheDir()
		if err != nil {
			return err
		}
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	canonical := absolute
	var suffix []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(canonical)
		if resolveErr == nil {
			canonical = filepath.Join(append([]string{resolved}, suffix...)...)
			break
		}
		if !os.IsNotExist(resolveErr) || filepath.Dir(canonical) == canonical {
			return resolveErr
		}
		suffix = append([]string{filepath.Base(canonical)}, suffix...)
		canonical = filepath.Dir(canonical)
	}
	socket := sharedRuntimeSocketPath(canonical, strings.Repeat("0", 64))
	limit := sharedRuntimeSocketPathLimit - (len(socket) - len(canonical))
	if len(socket) > sharedRuntimeSocketPathLimit {
		return sharedRuntimeError("broker_rendezvous_path_too_long", fmt.Errorf("cache root is %d bytes; limit is %d bytes (socket is %d bytes; limit is %d bytes)", len(canonical), limit, len(socket), sharedRuntimeSocketPathLimit))
	}
	return nil
}
