//go:build darwin || linux

package infra

import (
	"os"
	"syscall"
)

var engineWeightsCacheUID = os.Getuid

func secureWeightsCacheInfo(info os.FileInfo, directory bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	mode := os.FileMode(0600)
	if directory {
		mode = 0700
	}
	return ok && stat.Uid == uint32(engineWeightsCacheUID()) && info.Mode().Perm() == mode && info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 && info.IsDir() == directory && (directory || info.Mode().IsRegular())
}

func engineWeightsCacheRoot(resolved sharedResolvedProfile) string {
	return resolved.Paths.CanonicalCacheRoot
}
