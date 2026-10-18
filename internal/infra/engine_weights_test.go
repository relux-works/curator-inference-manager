package infra

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The launch verifier accepts the independent digest, refuses different bytes,
// and the ready-time guard rejects replacement even with identical contents.
func TestEngineWeightsLaunchVerifierAndIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.gguf")
	data := minimalWeightsGGUF(t, 3, false)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	actual := fmt.Sprintf("gguf-sha256:%x", sha256.Sum256(data))
	entry := EngineCatalogEntry{Weights: &EngineCatalogWeights{ID: actual}, Model: path}
	resolved := sharedResolvedProfile{CatalogObservation: &entry}
	observed, identity, err := verifyCatalogWeights(resolved)
	if err != nil || observed.ID != actual {
		t.Fatalf("verify=%+v %v", observed, err)
	}
	if err := identity.unchanged(); err != nil {
		t.Fatal(err)
	}
	replacement := path + ".replacement"
	if err := os.WriteFile(replacement, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	requireWeightsCode(t, identity.unchanged(), "engine_weights_file_changed")
	entry.Weights.ID = "gguf-sha256:" + strings.Repeat("a", 64)
	_, _, err = verifyCatalogWeights(resolved)
	requireWeightsCode(t, err, "engine_weights_mismatch")
	if !strings.Contains(err.Error(), actual) || !strings.Contains(err.Error(), entry.Weights.ID) {
		t.Fatal(err)
	}
}

// Size, mtime, disappearance and inode changes each invalidate the hash-time
// identity; unchanged metadata is the nearby positive control for every case.
func TestEngineWeightsIdentityChanges(t *testing.T) {
	for _, name := range []string{"size", "mtime", "absent", "same_bytes_new_inode"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "model.gguf")
			if err := os.WriteFile(path, minimalWeightsGGUF(t, 3, false), 0600); err != nil {
				t.Fatal(err)
			}
			_, identity, err := hashEngineWeights(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := identity.unchanged(); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "size":
				err = os.WriteFile(path, []byte("longer bytes"), 0600)
			case "mtime":
				stamp := identity.info.ModTime().Add(time.Second)
				err = os.Chtimes(path, stamp, stamp)
			case "absent":
				err = os.Remove(path)
			case "same_bytes_new_inode":
				err = os.WriteFile(path+".new", minimalWeightsGGUF(t, 3, false), 0600)
				if err == nil {
					err = os.Rename(path+".new", path)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			requireWeightsCode(t, identity.unchanged(), "engine_weights_file_changed")
		})
	}
}

// Shared admission uses the instance observation, refuses pin/recorded/actual
// disagreements and missing certification, and never opens the model pathname.
func TestEngineWeightsSharedAdmission(t *testing.T) {
	id := "gguf-sha256:" + strings.Repeat("a", 64)
	different := "gguf-sha256:" + strings.Repeat("b", 64)
	entry := EngineCatalogEntry{Model: "unavailable.gguf", Weights: &EngineCatalogWeights{ID: id}}
	resolved := sharedResolvedProfile{CatalogObservation: &entry, ExpectedWeightsID: id}
	observed := &EngineWeightsObservation{ID: id, ObservedAt: time.Now()}
	if err := validateExpectedEngineWeights(resolved, observed); err != nil {
		t.Fatal(err)
	}
	resolved.ExpectedWeightsID = different
	requireWeightsCode(t, validateExpectedEngineWeights(resolved, observed), "engine_weights_mismatch")
	resolved.ExpectedWeightsID = id
	observed.ID = different
	requireWeightsCode(t, validateExpectedEngineWeights(resolved, observed), "engine_weights_mismatch")
	requireWeightsCode(t, validateExpectedEngineWeights(resolved, nil), "engine_weights_unverified")
	entry.Weights = nil
	requireWeightsCode(t, validateExpectedEngineWeights(resolved, observed), "engine_weights_unpinned")
	resolved.ExpectedWeightsID = ""
	if err := validateExpectedEngineWeights(resolved, nil); err != nil {
		t.Fatal(err)
	}
}

func requireWeightsCode(t *testing.T, err error, code string) {
	t.Helper()
	var refusal *SharedRuntimeError
	if !errors.As(err, &refusal) || refusal.Code != code {
		t.Fatalf("error=%v want %s", err, code)
	}
}

// The legacy importer preserves absence of weights and never invents an id.
func TestEngineWeightsLegacyImportUnpinned(t *testing.T) {
	data, err := os.ReadFile("testdata/legacy-model-harness-fixtures.toml")
	if err != nil {
		t.Fatal(err)
	}
	document, _, err := ReadLegacyEngineProfiles(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Engines) == 0 {
		t.Fatal("legacy fixture has no entries")
	}
	for name, entry := range document.Engines {
		if entry.Weights != nil {
			t.Fatalf("import invented weights for %s", name)
		}
	}
}

func minimalWeightsGGUF(t *testing.T, version uint32, singleCount bool) []byte {
	t.Helper()
	var data bytes.Buffer
	data.WriteString("GGUF")
	count := uint64(0)
	if singleCount {
		count = 1
	}
	for _, field := range []any{version, uint64(0), count} {
		if err := binary.Write(&data, binary.LittleEndian, field); err != nil {
			t.Fatal(err)
		}
	}
	if singleCount {
		for _, field := range []any{uint64(len("split.count")), []byte("split.count"), uint32(2), uint16(1)} {
			if err := binary.Write(&data, binary.LittleEndian, field); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Extra bytes verify that hashing continues past metadata to EOF.
	data.WriteString("fixture payload")
	return data.Bytes()
}

// Minimal v2/v3 headers, including explicit split.count=1, hash every byte to
// an independent digest. Filenames and KV-estimate architecture limits do not
// determine whether metadata declares a split.
func TestEngineWeightsMinimalGGUFDigest(t *testing.T) {
	for _, version := range []uint32{2, 3} {
		for _, singleCount := range []bool{false, true} {
			t.Run(fmt.Sprintf("v%d/count_%t", version, singleCount), func(t *testing.T) {
				data := minimalWeightsGGUF(t, version, singleCount)
				path := filepath.Join(t.TempDir(), "model-00001-of-00002.gguf")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				got, identity, err := hashEngineWeights(path)
				want := fmt.Sprintf("gguf-sha256:%x", sha256.Sum256(data))
				if err != nil || got == nil || got.ID != want || identity == nil {
					t.Fatalf("hash=%+v identity=%+v err=%v want=%s", got, identity, err, want)
				}
				if err := identity.unchanged(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
