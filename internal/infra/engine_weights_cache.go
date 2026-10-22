package infra

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type engineWeightsCacheKey struct {
	Path                   string
	Device, Inode          uint64
	Size, MTimeNS, CTimeNS int64
}

type engineWeightsCacheEntry struct {
	Key engineWeightsCacheKey
	ID  string
}

var engineWeightsHasher = hashEngineWeights

func cachedEngineWeights(path, root string) (*EngineWeightsObservation, *engineWeightsFile, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || root == "" {
		return engineWeightsHasher(path)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return engineWeightsHasher(path)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return engineWeightsHasher(path)
	}
	key, supported := engineWeightsKey(resolved, info)
	if !supported {
		return engineWeightsHasher(path)
	}
	dir := filepath.Join(root, "weights-verification")
	name := filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(resolved))))
	// Owner/mode/no-symlink checks defend against other users; exact keys detect
	// accidental changes within filesystem timestamp resolution. HFS+ and other
	// 1-second filesystems can miss same-size rewrites within one second. Matching private
	// entries are authoritative, including same-uid edits. No same-uid integrity
	// guarantee is possible here; bypass with ensure --no-weights-cache.
	if secureWeightsCachePath(dir, true) && secureWeightsCachePath(name, false) {
		file, openErr := os.Open(name)
		if openErr == nil {
			stat, statErr := file.Stat()
			var entry engineWeightsCacheEntry
			decoder := json.NewDecoder(io.LimitReader(file, 4096))
			decodeErr := decoder.Decode(&entry)
			var trailing any
			if decodeErr == nil && decoder.Decode(&trailing) != io.EOF {
				decodeErr = io.ErrUnexpectedEOF
			}
			_ = file.Close()
			if statErr == nil && stat.Size() <= 4096 && secureWeightsCacheInfo(stat, false) && secureWeightsCachePath(name, false) && decodeErr == nil && entry.Key == key && engineWeightsIDPattern.MatchString(entry.ID) {
				identity := &engineWeightsFile{path: path, info: info}
				if err := identity.unchanged(); err != nil {
					return nil, nil, err
				}
				return &EngineWeightsObservation{ID: entry.ID, ObservedAt: time.Now().UTC()}, identity, nil
			}
		}
	}
	observed, identity, err := engineWeightsHasher(resolved)
	if err != nil {
		return observed, identity, err
	}
	identity.path = path
	if err := identity.unchanged(); err != nil {
		return nil, nil, err
	}
	hashedKey, ok := engineWeightsKey(resolved, identity.info)
	if !ok {
		return observed, identity, nil
	}
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return observed, identity, nil
	}
	if !secureWeightsCachePath(dir, true) {
		return observed, identity, nil
	}
	file, err := os.CreateTemp(dir, ".digest-")
	if err != nil {
		return observed, identity, nil
	}
	temp := file.Name()
	defer func() { _ = os.Remove(temp) }()
	err = json.NewEncoder(file).Encode(engineWeightsCacheEntry{Key: hashedKey, ID: observed.ID})
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil && closeErr == nil {
		_ = os.Rename(temp, name)
	}
	return observed, identity, nil
}

func secureWeightsCachePath(path string, directory bool) bool {
	info, err := os.Lstat(path)
	return err == nil && secureWeightsCacheInfo(info, directory)
}
