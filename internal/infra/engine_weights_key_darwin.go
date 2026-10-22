package infra

import (
	"os"
	"syscall"
)

func engineWeightsKey(path string, info os.FileInfo) (engineWeightsCacheKey, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() {
		return engineWeightsCacheKey{}, false
	}
	return engineWeightsCacheKey{path, uint64(stat.Dev), uint64(stat.Ino), info.Size(), info.ModTime().UnixNano(), stat.Ctimespec.Sec*1e9 + stat.Ctimespec.Nsec}, true
}
