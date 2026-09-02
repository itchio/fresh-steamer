package cdn

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"math/rand"
	"testing"

	"github.com/itchio/fresh-steamer/steamcrypto"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz/lzma"
)

var testKey = bytes.Repeat([]byte{0x42}, 32)

func samplePayload() []byte {
	r := rand.New(rand.NewSource(1))
	b := make([]byte, 50000)
	for i := range b {
		// Compressible but not trivial.
		b[i] = byte(r.Intn(16)) + 'a'
	}
	return b
}

func vzipFrame(t *testing.T, plain []byte) []byte {
	var buf bytes.Buffer
	w, err := lzma.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(plain)
	w.Close()
	// Classic .lzma output is 5 property bytes, 8 size bytes, then stream.
	stream := buf.Bytes()
	props, body := stream[:5], stream[13:]

	out := []byte{'V', 'Z', 'a'}
	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(plain))
	out = append(out, props...)
	out = append(out, body...)
	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(plain))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(plain)))
	return append(out, 'z', 'v')
}

func vzstdFrame(t *testing.T, plain []byte) []byte {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	body := enc.EncodeAll(plain, nil)
	out := []byte{'V', 'S', 'Z', 'a'}
	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(plain))
	out = append(out, body...)
	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(plain))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(plain)))
	out = append(out, 0, 0, 0, 0)
	return append(out, 'z', 's', 'v')
}

func zipFrame(t *testing.T, plain []byte) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create("z")
	if err != nil {
		t.Fatal(err)
	}
	f.Write(plain)
	w.Close()
	return buf.Bytes()
}

func encrypt(t *testing.T, frame []byte) []byte {
	enc, err := steamcrypto.SymmetricEncrypt(frame, testKey, bytes.Repeat([]byte{3}, 16))
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func TestDecodeChunkFormats(t *testing.T) {
	plain := samplePayload()
	cases := map[string][]byte{
		"vzip":  vzipFrame(t, plain),
		"vzstd": vzstdFrame(t, plain),
		"zip":   zipFrame(t, plain),
	}
	for name, frame := range cases {
		got, err := DecodeChunk(encrypt(t, frame), testKey)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("%s: payload mismatch", name)
		}
	}
}

func TestDecodeChunkRejectsGarbage(t *testing.T) {
	if _, err := DecodeChunk(encrypt(t, []byte("not a chunk at all")), testKey); err == nil {
		t.Fatal("expected error")
	}
	if _, err := DecodeChunk([]byte("short"), testKey); err == nil {
		t.Fatal("expected error")
	}
	bad := vzstdFrame(t, samplePayload())
	bad[len(bad)-1] = 'x'
	if _, err := DecodeChunk(encrypt(t, bad), testKey); err == nil {
		t.Fatal("expected footer error")
	}
}
