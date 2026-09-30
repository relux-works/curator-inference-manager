package infra

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
)

type engineGGUF struct {
	arch   string
	values map[string]any
}

func (g engineGGUF) number(key string) uint64 {
	value, _ := g.values[key].(uint64)
	return value
}

// The reader stops at the last metadata value, before tensor descriptors or data.
// No buffering or mmap can prefetch tensor bytes. Limit all untrusted header work.
func readEngineGGUF(path string) (engineGGUF, string) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return engineGGUF{}, "model_absent"
	}
	if err != nil {
		return engineGGUF{}, "gguf_read_failed"
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return engineGGUF{}, "gguf_read_failed"
	}
	g, err := decodeEngineGGUF(io.LimitReader(f, 32<<20))
	if err != nil {
		var pathError *os.PathError
		if errors.As(err, &pathError) {
			return engineGGUF{}, "gguf_read_failed"
		}
		return engineGGUF{}, "gguf_invalid"
	}
	switch g.arch {
	case "llama", "qwen2", "qwen3":
	default:
		return engineGGUF{}, "gguf_architecture_unsupported"
	}
	if _, exists := g.values[g.arch+".attention.sliding_window"]; exists {
		return engineGGUF{}, "gguf_architecture_unsupported"
	}
	return g, ""
}

func decodeEngineGGUF(r io.Reader) (engineGGUF, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return engineGGUF{}, err
	}
	if string(magic[:]) != "GGUF" {
		return engineGGUF{}, ggufError("invalid GGUF magic")
	}
	var version uint32
	var tensors, count uint64
	for _, v := range []any{&version, &tensors, &count} {
		if err := binary.Read(r, binary.LittleEndian, v); err != nil {
			return engineGGUF{}, err
		}
	}
	if (version != 2 && version != 3) || count > 1<<20 {
		return engineGGUF{}, ggufError("unsupported GGUF header")
	}
	g := engineGGUF{values: map[string]any{}}
	for i := uint64(0); i < count; i++ {
		key, err := engineGGUFString(r)
		if err != nil {
			return g, err
		}
		if _, exists := g.values[key]; exists {
			return g, ggufError("duplicate GGUF key")
		}
		var kind uint32
		if err := binary.Read(r, binary.LittleEndian, &kind); err != nil {
			return g, err
		}
		value, err := engineGGUFValue(r, kind, true)
		if err != nil {
			return g, err
		}
		g.values[key] = value
	}
	g.arch, _ = g.values["general.architecture"].(string)
	return g, nil
}

func engineGGUFString(r io.Reader) (string, error) {
	var length uint64
	if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
		return "", err
	}
	if length > 1<<20 {
		return "", ggufError("GGUF string exceeds header limit")
	}
	data := make([]byte, int(length))
	_, err := io.ReadFull(r, data)
	return string(data), err
}

func engineGGUFValue(r io.Reader, kind uint32, allowArray bool) (any, error) {
	switch kind {
	case 8:
		return engineGGUFString(r)
	case 9:
		var element uint32
		var count uint64
		if err := binary.Read(r, binary.LittleEndian, &element); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
			return nil, err
		}
		if !allowArray || element == 9 || count > 1<<20 {
			return nil, ggufError("unsupported GGUF array")
		}
		for i := uint64(0); i < count; i++ {
			if _, err := engineGGUFValue(r, element, false); err != nil {
				return nil, err
			}
		}
		return struct{}{}, nil
	}
	sizes := map[uint32]int{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8}
	size, ok := sizes[kind]
	if !ok {
		return nil, ggufError("unknown GGUF value type")
	}
	var data [8]byte
	if _, err := io.ReadFull(r, data[:size]); err != nil {
		return nil, err
	}
	var value uint64
	switch size {
	case 1:
		value = uint64(data[0])
	case 2:
		value = uint64(binary.LittleEndian.Uint16(data[:2]))
	case 4:
		value = uint64(binary.LittleEndian.Uint32(data[:4]))
	case 8:
		value = binary.LittleEndian.Uint64(data[:])
	}
	// Floats, booleans and negative signed integers are not dimensions.
	if kind == 6 || kind == 7 || kind == 12 || ((kind == 1 || kind == 3 || kind == 5 || kind == 11) && data[size-1]&0x80 != 0) {
		return struct{}{}, nil
	}
	return value, nil
}
