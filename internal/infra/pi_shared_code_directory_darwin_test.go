//go:build darwin

package infra

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"os"
	"testing"
)

func sharedTestCodeDirectory(hashType byte) []byte {
	size := byte(32)
	if hashType == 1 || hashType == 3 {
		size = 20
	}
	if hashType == 4 {
		size = 48
	}
	cd := make([]byte, max(112, 48+int(size)))
	put := binary.BigEndian.PutUint32
	put(cd[:4], 0xfade0c02)
	put(cd[4:8], uint32(len(cd)))
	put(cd[8:12], 0x20001)
	put(cd[16:20], 48)
	put(cd[20:24], 44)
	put(cd[28:32], 1)
	put(cd[32:36], 1)
	cd[36], cd[37], cd[39] = size, hashType, 12
	copy(cd[44:], "id\x00")
	return cd
}
func sharedTestSuperBlob(directories ...[]byte) []byte {
	size := 12 + 8*len(directories)
	for _, cd := range directories {
		size += len(cd)
	}
	blob := make([]byte, size)
	put := binary.BigEndian.PutUint32
	put(blob[:4], 0xfade0cc0)
	put(blob[4:8], uint32(size))
	put(blob[8:12], uint32(len(directories)))
	offset := 12 + 8*len(directories)
	for i, cd := range directories {
		slot := uint32(0)
		if i > 0 {
			slot = 0x1000 + uint32(i-1)
		}
		put(blob[12+8*i:16+8*i], slot)
		put(blob[16+8*i:20+8*i], uint32(offset))
		copy(blob[offset:], cd)
		offset += len(cd)
	}
	return blob
}

// HashType selects the digest; SuperBlob selection follows XNU ranks in either order.
func TestSharedCodeDirectoryHashSelection(t *testing.T) {
	for _, typ := range []byte{1, 2, 3, 4} {
		cd := sharedTestCodeDirectory(typ)
		var want sharedCodeHash
		switch typ {
		case 1:
			h := sha1.Sum(cd)
			copy(want[:], h[:])
		case 2, 3:
			h := sha256.Sum256(cd)
			copy(want[:], h[:20])
		case 4:
			h := sha512.Sum384(cd)
			copy(want[:], h[:20])
		}
		for _, blob := range [][]byte{cd, sharedTestSuperBlob(cd)} {
			if got, err := sharedSignatureCodeHash(blob); err != nil || got != want {
				t.Fatalf("type=%d got=%x want=%x err=%v", typ, got, want, err)
			}
		}
	}
	sha1CD, sha256CD, sha384CD := sharedTestCodeDirectory(1), sharedTestCodeDirectory(2), sharedTestCodeDirectory(4)
	for _, blobs := range [][][]byte{{sha1CD, sha256CD}, {sha256CD, sha1CD}, {sha1CD, sharedTestCodeDirectory(3), sha256CD}, {sha256CD, sha384CD}} {
		wantCD := sha256CD
		if len(blobs) == 2 && blobs[1][37] == 4 {
			wantCD = sha384CD
		}
		want, err := sharedSignatureCodeHash(wantCD)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := sharedSignatureCodeHash(sharedTestSuperBlob(blobs...)); err != nil || got != want {
			t.Fatalf("selection=%x want=%x err=%v", got, want, err)
		}
	}
}

// XNU's 112-byte sizeof(CS_CodeDirectory) floor also applies to the oldest version.
// Both boundary fixtures otherwise contain the same in-bounds identifier and hash slot.
func TestSharedCodeDirectoryMinimumLength(t *testing.T) {
	for _, size := range []int{111, 112} {
		cd := append([]byte(nil), sharedTestCodeDirectory(2)[:size]...)
		binary.BigEndian.PutUint32(cd[4:8], uint32(size))
		want := sha256.Sum256(cd)
		for _, blob := range [][]byte{cd, sharedTestSuperBlob(cd)} {
			got, err := sharedSignatureCodeHash(blob)
			if size == 111 {
				if err == nil || got != (sharedCodeHash{}) {
					t.Fatalf("minimum-1: hash=%x err=%v", got, err)
				}
			} else if err != nil || got != sharedCodeHash(want[:20]) {
				t.Fatalf("minimum: hash=%x want=%x err=%v", got, want[:20], err)
			}
		}
	}
}

// Bounds, versions, slot sizes, missing/duplicate directories and unsupported hashes refuse.
func TestSharedCodeDirectoryRejectsMalformed(t *testing.T) {
	valid := sharedTestSuperBlob(sharedTestCodeDirectory(2))
	if _, err := sharedSignatureCodeHash(valid); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{"empty": nil, "truncated header": valid[:7], "truncated blob": valid[:len(valid)-1], "no directory": sharedTestSuperBlob(), "equal rank": sharedTestSuperBlob(sharedTestCodeDirectory(2), sharedTestCodeDirectory(2)), "unknown hash": sharedTestSuperBlob(sharedTestCodeDirectory(99))}
	for name, mutate := range map[string]func([]byte){
		"index overflow":              func(b []byte) { binary.BigEndian.PutUint32(b[8:12], 0xffffffff) },
		"offset overflow":             func(b []byte) { binary.BigEndian.PutUint32(b[16:20], 0xffffffff) },
		"offset into index":           func(b []byte) { binary.BigEndian.PutUint32(b[16:20], 12) },
		"bad child magic":             func(b []byte) { b[20] = 0 },
		"short child":                 func(b []byte) { binary.BigEndian.PutUint32(b[24:28], 8) },
		"unknown version":             func(b []byte) { binary.BigEndian.PutUint32(b[28:32], 0x20700) },
		"bad hash size":               func(b []byte) { b[20+36] = 1 },
		"hash beyond directory":       func(b []byte) { binary.BigEndian.PutUint32(b[20+16:20+20], 0xffffffff) },
		"special slots overflow":      func(b []byte) { binary.BigEndian.PutUint32(b[20+24:20+28], 0xffffffff) },
		"code slots overflow":         func(b []byte) { binary.BigEndian.PutUint32(b[20+28:20+32], 0xffffffff) },
		"identifier beyond directory": func(b []byte) { binary.BigEndian.PutUint32(b[20+20:20+24], 0xffffffff) },
	} {
		b := append([]byte(nil), valid...)
		mutate(b)
		cases[name] = b
	}
	for name, blob := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := sharedSignatureCodeHash(blob); err == nil {
				t.Fatal("malformed signature admitted")
			}
		})
	}
}

// Each extension has a valid control and rejects its own truncated header.
// Variable scatter/team/linkage data obey XNU bounds; numeric file offsets are not blob offsets.
func TestSharedCodeDirectoryExtensionBounds(t *testing.T) {
	put := binary.BigEndian.PutUint32
	fixture := func(version uint32) []byte {
		cd := make([]byte, 256)
		copy(cd, sharedTestCodeDirectory(2)[:44])
		put(cd[4:8], uint32(len(cd)))
		put(cd[8:12], version)
		put(cd[16:20], 224)
		put(cd[20:24], 108)
		copy(cd[108:], "id\x00")
		return cd
	}
	for _, extension := range []struct {
		name    string
		version uint32
		size    int
	}{
		{"scatter", 0x20100, 48}, {"team", 0x20200, 52}, {"codeLimit64", 0x20300, 64},
		{"execSeg", 0x20400, 88}, {"runtime", 0x20500, 96}, {"linkage", 0x20600, 108},
	} {
		t.Run(extension.name, func(t *testing.T) {
			cd := fixture(extension.version)
			if _, err := sharedSignatureCodeHash(cd); err != nil {
				t.Fatal(err)
			}
			short := append([]byte(nil), cd[:extension.size-1]...)
			put(short[4:8], uint32(len(short)))
			if _, err := sharedSignatureCodeHash(short); err == nil {
				t.Fatal("truncated extension accepted")
			}
		})
	}
	for name, mutate := range map[string]func([]byte){
		"scatter offset":         func(cd []byte) { put(cd[44:48], 128) },
		"team string":            func(cd []byte) { put(cd[48:52], 128); copy(cd[128:], "team\x00") },
		"linkage":                func(cd []byte) { cd[96] = 2; put(cd[100:104], 224); put(cd[104:108], 32) },
		"empty linkage at end":   func(cd []byte) { cd[96] = 2; put(cd[100:104], 256) },
		"unused linkage offsets": func(cd []byte) { put(cd[100:104], 0xffffffff); put(cd[104:108], 0xffffffff) },
		"numeric extensions": func(cd []byte) {
			for i := 56; i < 96; i++ {
				cd[i] = 0xff
			}
		},
	} {
		t.Run("valid "+name, func(t *testing.T) {
			cd := fixture(0x20600)
			mutate(cd)
			if _, err := sharedSignatureCodeHash(cd); err != nil {
				t.Fatal(err)
			}
		})
	}
	for name, mutate := range map[string]func([]byte){
		"scatter past end":         func(cd []byte) { put(cd[44:48], 0xffffffff) },
		"scatter truncated entry":  func(cd []byte) { put(cd[44:48], 233) },
		"scatter missing sentinel": func(cd []byte) { put(cd[44:48], 232); put(cd[232:236], 1) },
		"scatter page overflow":    func(cd []byte) { put(cd[44:48], 128); put(cd[128:132], 0xffffffff); put(cd[152:156], 1) },
		"team past end":            func(cd []byte) { put(cd[48:52], 0xffffffff) },
		"team at end":              func(cd []byte) { put(cd[48:52], 256) },
		"team unterminated":        func(cd []byte) { put(cd[48:52], 255); cd[255] = 1 },
		"linkage offset":           func(cd []byte) { cd[96] = 2; put(cd[100:104], 0xffffffff) },
		"linkage size":             func(cd []byte) { cd[96] = 2; put(cd[100:104], 224); put(cd[104:108], 33) },
		"linkage overflow":         func(cd []byte) { cd[96] = 2; put(cd[100:104], 0xffffffff); put(cd[104:108], 0xffffffff) },
		"page zero":                func(cd []byte) { cd[39] = 0 },
		"page shift 11":            func(cd []byte) { cd[39] = 11 },
		"page shift 13":            func(cd []byte) { cd[39] = 13 },
		"page shift 255":           func(cd []byte) { cd[39] = 255 },
	} {
		t.Run(name, func(t *testing.T) {
			cd := fixture(0x20600)
			mutate(cd)
			if _, err := sharedSignatureCodeHash(cd); err == nil {
				t.Fatal("malformed extension accepted")
			}
		})
	}
	for _, shift := range []byte{12, 14} {
		cd := fixture(0x20600)
		cd[39] = shift
		_, err := sharedSignatureCodeHash(cd)
		valid := shift == 12 || (1<<shift) == os.Getpagesize()
		if (err == nil) != valid {
			t.Fatalf("page shift %d: valid=%t err=%v", shift, valid, err)
		}
	}
}
