//go:build darwin

package infra

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"debug/macho"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// sharedFileCodeHash reads an expected file, never a peer's executable path.
// It identifies its embedded CodeDirectory, not its publisher or validity.
func sharedFileCodeHash(path, arch string) (sharedCodeHash, error) {
	file, err := os.Open(path)
	if err != nil {
		return sharedCodeHash{}, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return sharedCodeHash{}, err
	}
	var cpu macho.Cpu
	switch arch {
	case "arm64":
		cpu = macho.CpuArm64
	case "amd64":
		cpu = macho.CpuAmd64
	default:
		return sharedCodeHash{}, errors.New("unsupported code identity architecture")
	}
	var reader io.ReaderAt = io.NewSectionReader(file, 0, stat.Size())
	fat, err := macho.NewFatFile(reader)
	if err == nil {
		found := false
		for _, slice := range fat.Arches {
			if slice.Cpu != cpu {
				continue
			}
			if found {
				return sharedCodeHash{}, errors.New("ambiguous executable architecture")
			}
			if uint64(slice.Offset)+uint64(slice.Size) > uint64(stat.Size()) {
				return sharedCodeHash{}, errors.New("truncated executable slice")
			}
			reader = io.NewSectionReader(file, int64(slice.Offset), int64(slice.Size))
			found = true
		}
		if !found {
			return sharedCodeHash{}, errors.New("executable architecture absent")
		}
	} else if !errors.Is(err, macho.ErrNotFat) {
		return sharedCodeHash{}, err
	}
	image, err := macho.NewFile(reader)
	if err != nil {
		return sharedCodeHash{}, err
	}
	if image.Cpu != cpu || image.Type != macho.TypeExec {
		return sharedCodeHash{}, errors.New("foreign executable architecture or type")
	}
	var signature []byte
	for _, load := range image.Loads {
		raw := load.Raw()
		if len(raw) < 8 || image.ByteOrder.Uint32(raw[:4]) != 0x1d {
			continue
		} // LC_CODE_SIGNATURE
		if len(raw) != 16 || signature != nil {
			return sharedCodeHash{}, errors.New("invalid or duplicate code signature command")
		}
		offset, size := image.ByteOrder.Uint32(raw[8:12]), image.ByteOrder.Uint32(raw[12:16])
		if size < 8 || size > 16<<20 {
			return sharedCodeHash{}, errors.New("invalid code signature size")
		}
		signature = make([]byte, int(size))
		if _, err := reader.ReadAt(signature, int64(offset)); err != nil {
			return sharedCodeHash{}, err
		}
	}
	return sharedSignatureCodeHash(signature)
}

// Selection follows XNU cs_validate_csblob/hash_rank: SHA-384 > SHA-256 >
// truncated SHA-256 > SHA-1; an equal best rank is rejected, not guessed.
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/ubc_subr.c
func sharedSignatureCodeHash(signature []byte) (sharedCodeHash, error) {
	bad := errors.New("malformed or unsupported embedded code signature")
	be := binary.BigEndian
	blob := func(data []byte) ([]byte, bool) {
		if len(data) < 8 {
			return nil, false
		}
		size := uint64(be.Uint32(data[4:8]))
		if size < 8 || size > uint64(len(data)) {
			return nil, false
		}
		return data[:size], true
	}
	signature, ok := blob(signature)
	if !ok {
		return sharedCodeHash{}, bad
	}
	var directories [][]byte
	switch be.Uint32(signature[:4]) {
	case 0xfade0c02:
		directories = append(directories, signature)
	case 0xfade0cc0:
		if len(signature) < 12 {
			return sharedCodeHash{}, bad
		}
		count := uint64(be.Uint32(signature[8:12]))
		if count > uint64(len(signature)-12)/8 {
			return sharedCodeHash{}, bad
		}
		for i := uint64(0); i < count; i++ {
			index := signature[12+i*8 : 20+i*8]
			slot, offset := be.Uint32(index[:4]), uint64(be.Uint32(index[4:]))
			if offset < 12+count*8 || offset > uint64(len(signature)) {
				return sharedCodeHash{}, bad
			}
			child, ok := blob(signature[offset:])
			if !ok {
				return sharedCodeHash{}, bad
			}
			if slot == 0 || (slot >= 0x1000 && slot < 0x1005) {
				directories = append(directories, child)
			}
		}
	default:
		return sharedCodeHash{}, bad
	}
	var result sharedCodeHash
	best := 0
	for _, cd := range directories {
		// XNU first requires sizeof(CS_CodeDirectory), including tail padding,
		// regardless of version; this is 112 bytes on Darwin's 64-bit ABIs.
		// https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.1.9/osfmk/kern/cs_blobs.h#L189
		// https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.1.9/bsd/kern/ubc_subr.c#L340
		if len(cd) < 112 || be.Uint32(cd[:4]) != 0xfade0c02 {
			return sharedCodeHash{}, bad
		}
		version := be.Uint32(cd[8:12])
		minimum := 44
		for _, extension := range []struct {
			version uint32
			size    int
		}{{0x20100, 48}, {0x20200, 52}, {0x20300, 64}, {0x20400, 88}, {0x20500, 96}, {0x20600, 108}} {
			if version >= extension.version {
				minimum = extension.size
			}
		}
		if version < 0x20001 || version > 0x20600 || len(cd) < minimum {
			return sharedCodeHash{}, bad
		}
		// Structural extension checks follow cs_validate_codedirectory in XNU.
		// codeLimit64/execSeg/runtime are fixed-width values, not blob offsets;
		// the version-specific minimum above bounds those entire extensions.
		// https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.1.9/bsd/kern/ubc_subr.c#L340
		length := uint64(len(cd))
		pageShift := cd[39]
		if pageShift != 12 && (pageShift >= 63 || uint64(1)<<pageShift != uint64(os.Getpagesize())) {
			return sharedCodeHash{}, bad
		}
		if version >= 0x20100 {
			offset := uint64(be.Uint32(cd[44:48]))
			if offset != 0 {
				var pages uint32
				for {
					if offset > length || length-offset < 24 {
						return sharedCodeHash{}, bad
					}
					count := be.Uint32(cd[offset : offset+4])
					if count == 0 {
						break
					}
					if pages+count < pages {
						return sharedCodeHash{}, bad
					}
					pages += count
					offset += 24
				}
			}
		}
		if version >= 0x20200 {
			offset := uint64(be.Uint32(cd[48:52]))
			if offset != 0 && (offset >= length || bytes.IndexByte(cd[offset:], 0) < 0) {
				return sharedCodeHash{}, bad
			}
		}
		if version >= 0x20600 && cd[96] != 0 {
			offset, size := uint64(be.Uint32(cd[100:104])), uint64(be.Uint32(cd[104:108]))
			if offset > length || size > length-offset {
				return sharedCodeHash{}, bad
			}
		}
		hashOffset, identifier := uint64(be.Uint32(cd[16:20])), uint64(be.Uint32(cd[20:24]))
		special, slots := uint64(be.Uint32(cd[24:28])), uint64(be.Uint32(cd[28:32]))
		hashSize, hashType := uint64(cd[36]), cd[37]
		rank, size := 0, uint64(0)
		var digest []byte
		switch hashType {
		case 1:
			rank, size = 1, 20
			h := sha1.Sum(cd)
			digest = h[:]
		case 3:
			rank, size = 2, 20
			h := sha256.Sum256(cd)
			digest = h[:]
		case 2:
			rank, size = 3, 32
			h := sha256.Sum256(cd)
			digest = h[:]
		case 4:
			rank, size = 4, 48
			h := sha512.Sum384(cd)
			digest = h[:]
		default:
			return sharedCodeHash{}, bad
		}
		if hashSize != size || hashOffset > uint64(len(cd)) || special*hashSize > hashOffset || hashOffset-special*hashSize < uint64(minimum) || slots*hashSize > uint64(len(cd))-hashOffset || identifier < uint64(minimum) || identifier >= uint64(len(cd)) || bytes.IndexByte(cd[identifier:], 0) < 0 {
			return sharedCodeHash{}, bad
		}
		if rank == best {
			return sharedCodeHash{}, bad
		}
		if rank > best {
			best = rank
			copy(result[:], digest[:20])
		}
	}
	if best == 0 {
		return sharedCodeHash{}, bad
	}
	return result, nil
}
