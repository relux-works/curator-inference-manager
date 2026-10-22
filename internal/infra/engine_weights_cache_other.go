//go:build !darwin && !linux

package infra

import "os"

func engineWeightsKey(string, os.FileInfo) (engineWeightsCacheKey, bool) {
	return engineWeightsCacheKey{}, false
}
func secureWeightsCacheInfo(os.FileInfo, bool) bool       { return false }
func engineWeightsCacheRoot(sharedResolvedProfile) string { return "" }
