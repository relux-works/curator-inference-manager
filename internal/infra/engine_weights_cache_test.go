//go:build darwin || linux

package infra

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Repeated ensure verification reuses a digest only for the exact file key.
// Every metadata/path change invalidates a poisoned digest before comparison.
func TestEngineWeightsVerificationCacheKey(t *testing.T) {
	for _, change := range []string{"size", "mtime", "ctime", "inode", "path", "device-key"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "model.gguf")
			data := minimalWeightsGGUF(t, 3, false)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			initial, _, err := hashEngineWeights(path)
			if err != nil {
				t.Fatal(err)
			}
			entry := EngineCatalogEntry{Model: path, Weights: &EngineCatalogWeights{ID: initial.ID}}
			resolved := sharedResolvedProfile{CatalogObservation: &entry, ExpectedWeightsID: initial.ID}
			resolved.Paths.CanonicalCacheRoot = root
			old := engineWeightsHasher
			count := 0
			engineWeightsHasher = func(path string) (*EngineWeightsObservation, *engineWeightsFile, error) {
				count++
				return hashEngineWeights(path)
			}
			t.Cleanup(func() { engineWeightsHasher = old })
			for i := 0; i < 2; i++ {
				observation, _, err := verifyCatalogWeights(resolved)
				if err != nil {
					t.Fatal(err)
				}
				if err := validateExpectedEngineWeights(resolved, observation); err != nil {
					t.Fatal(err)
				}
			}
			if count != 1 {
				t.Fatalf("unchanged verification hashes=%d", count)
			}
			files, err := filepath.Glob(filepath.Join(root, "weights-verification", "*.json"))
			if err != nil || len(files) != 1 {
				t.Fatalf("cache entries: %v %v", files, err)
			}
			raw, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var cached engineWeightsCacheEntry
			if err := json.Unmarshal(raw, &cached); err != nil {
				t.Fatal(err)
			}
			// A stale expected digest in the cache must not certify changed bytes.
			switch change {
			case "size":
				data = append(data, 1)
				err = os.WriteFile(path, data, 0600)
			case "mtime":
				stamp := time.Now().Add(time.Hour)
				err = os.Chtimes(path, stamp, stamp)
			case "ctime":
				info, statErr := os.Stat(path)
				if statErr != nil {
					t.Fatal(statErr)
				}
				data[len(data)-1] ^= 1
				err = os.WriteFile(path, data, 0600)
				if err == nil {
					err = os.Chtimes(path, info.ModTime(), info.ModTime())
				}
			case "inode":
				err = os.WriteFile(path+".new", data, 0600)
				if err == nil {
					err = os.Rename(path+".new", path)
				}
			case "path":
				entry.Model = filepath.Join(root, "renamed.gguf")
				err = os.Rename(path, entry.Model)
			case "device-key":
				cached.Key.Device++
				raw, err = json.Marshal(cached)
				if err == nil {
					err = os.WriteFile(files[0], raw, 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = verifyCatalogWeights(resolved)
			if change == "size" || change == "ctime" {
				requireWeightsCode(t, err, "engine_weights_mismatch")
			} else if err != nil {
				t.Fatal(err)
			}
			if count != 2 {
				t.Fatalf("%s hashes=%d want 2", change, count)
			}
			dirInfo, err := os.Stat(filepath.Dir(files[0]))
			if err != nil || dirInfo.Mode().Perm() != 0700 {
				t.Fatal("directory permissions", err)
			}
			fileInfo, err := os.Stat(files[0])
			if err != nil || fileInfo.Mode().Perm() != 0600 {
				t.Fatal("entry permissions", err)
			}
		})
	}
}

// Unsafe entries and directories are cache misses, never verification failures
// or authority to accept a different expected digest. Valid entries are controls.
func TestEngineWeightsVerificationCacheUntrusted(t *testing.T) {
	for _, change := range []string{"mode", "owner", "corrupt", "oversize", "unreadable", "directory-mode", "symlink", "wrong-key", "wrong-digest"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "model.gguf")
			if err := os.WriteFile(path, minimalWeightsGGUF(t, 3, false), 0600); err != nil {
				t.Fatal(err)
			}
			valid, _, err := cachedEngineWeights(path, root)
			if err != nil {
				t.Fatal(err)
			}
			files, err := filepath.Glob(filepath.Join(root, "weights-verification", "*.json"))
			if err != nil || len(files) != 1 {
				t.Fatal(files, err)
			}
			raw, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var cached engineWeightsCacheEntry
			if err := json.Unmarshal(raw, &cached); err != nil {
				t.Fatal(err)
			}
			cached.ID = "gguf-sha256:" + strings.Repeat("a", 64)
			if change == "wrong-key" {
				cached.Key.Size++
			}
			raw, err = json.Marshal(cached)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(files[0], raw, 0600); err != nil {
				t.Fatal(err)
			}
			oldHash, oldUID := engineWeightsHasher, engineWeightsCacheUID
			count := 0
			engineWeightsHasher = func(path string) (*EngineWeightsObservation, *engineWeightsFile, error) {
				count++
				return hashEngineWeights(path)
			}
			t.Cleanup(func() { engineWeightsHasher, engineWeightsCacheUID = oldHash, oldUID })
			switch change {
			case "mode":
				err = os.Chmod(files[0], 0644)
			case "owner":
				// Admit the private directory, then simulate a foreign entry owner.
				checks := 0
				engineWeightsCacheUID = func() int {
					checks++
					if checks == 1 {
						return os.Getuid()
					}
					return os.Getuid() + 1
				}
			case "unreadable":
				err = os.Chmod(files[0], 0000)
			case "directory-mode":
				err = os.Chmod(filepath.Dir(files[0]), 0755)
			case "oversize":
				err = os.WriteFile(files[0], append(raw, []byte(strings.Repeat(" ", 4096)+"!")...), 0600)
			case "corrupt":
				err = os.WriteFile(files[0], []byte("{"), 0600)
			case "symlink":
				err = os.Rename(files[0], files[0]+".target")
				if err == nil {
					err = os.Symlink(files[0]+".target", files[0])
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			observation, _, err := cachedEngineWeights(path, root)
			if err != nil {
				t.Fatal(err)
			}
			if change == "wrong-digest" {
				// Same-owner private cache contents are trusted; an independent operator
				// pin still rejects a cached digest that differs from that pin.
				entry := EngineCatalogEntry{Weights: &EngineCatalogWeights{ID: valid.ID}}
				requireWeightsCode(t, validateExpectedEngineWeights(sharedResolvedProfile{CatalogObservation: &entry, ExpectedWeightsID: valid.ID}, observation), "engine_weights_mismatch")
			} else if observation.ID != valid.ID || count != 1 {
				t.Fatalf("untrusted %s: %s hashes=%d", change, observation.ID, count)
			}
		})
	}
}

// This pins the intended trust boundary: a matching same-owner 0600 entry is
// authoritative even when forged; opting out rehashes and refuses the mismatch.
func TestEngineWeightsVerificationCacheOptOut(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "model.gguf")
	if err := os.WriteFile(path, minimalWeightsGGUF(t, 3, false), 0600); err != nil {
		t.Fatal(err)
	}
	actual, _, err := cachedEngineWeights(path, root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(root, "weights-verification", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var forged engineWeightsCacheEntry
	if err := json.Unmarshal(raw, &forged); err != nil {
		t.Fatal(err)
	}
	forged.ID = "gguf-sha256:" + strings.Repeat("a", 64)
	if forged.ID == actual.ID {
		t.Fatal("fixture digest must differ from forged pin")
	}
	raw, err = json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[0], raw, 0600); err != nil {
		t.Fatal(err)
	}
	entry := EngineCatalogEntry{Model: path, Weights: &EngineCatalogWeights{ID: forged.ID}}
	resolved := sharedResolvedProfile{CatalogObservation: &entry, ExpectedWeightsID: forged.ID}
	resolved.Paths.CanonicalCacheRoot = root
	old := engineWeightsHasher
	count := 0
	engineWeightsHasher = func(path string) (*EngineWeightsObservation, *engineWeightsFile, error) {
		count++
		return hashEngineWeights(path)
	}
	t.Cleanup(func() { engineWeightsHasher = old })
	observed, _, err := verifyCatalogWeights(resolved)
	if err != nil || observed.ID != forged.ID || count != 0 {
		t.Fatalf("matching private entry not honored: observation=%+v hashes=%d err=%v", observed, count, err)
	}
	resolved.NoWeightsCache = true
	observed, _, err = verifyCatalogWeights(resolved)
	requireWeightsCode(t, err, "engine_weights_mismatch")
	if observed.ID != actual.ID || count != 1 {
		t.Fatalf("opt-out did not hash actual bytes: observation=%+v hashes=%d", observed, count)
	}
	// Opt-out also succeeds for a valid pin and does not rewrite the cache.
	entry.Weights.ID, resolved.ExpectedWeightsID = actual.ID, actual.ID
	if _, _, err := verifyCatalogWeights(resolved); err != nil || count != 2 {
		t.Fatalf("valid opt-out control: hashes=%d err=%v", count, err)
	}
	after, err := os.ReadFile(files[0])
	if err != nil || string(after) != string(raw) {
		t.Fatalf("opt-out changed cache: %v", err)
	}
}
