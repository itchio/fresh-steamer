package cdn

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/itchio/fresh-steamer/steamcrypto"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz/lzma"
)

// FetchChunk downloads one chunk and returns its plain contents, verified
// against the manifest checksum.
func (c *Client) FetchChunk(ctx context.Context, depotID uint32, chunk *Chunk, depotKey []byte) ([]byte, error) {
	raw, err := c.get(ctx, fmt.Sprintf("/depot/%d/chunk/%s", depotID, hex.EncodeToString(chunk.SHA)))
	if err != nil {
		return nil, fmt.Errorf("fetching chunk %x: %w", chunk.SHA[:4], err)
	}
	data, err := DecodeChunk(raw, depotKey)
	if err != nil {
		return nil, fmt.Errorf("decoding chunk %x: %w", chunk.SHA[:4], err)
	}
	if uint32(len(data)) != chunk.Size {
		return nil, fmt.Errorf("chunk %x: got %d bytes, manifest says %d", chunk.SHA[:4], len(data), chunk.Size)
	}
	if sum := steamcrypto.Adler(data); sum != chunk.Checksum {
		return nil, fmt.Errorf("chunk %x: checksum mismatch", chunk.SHA[:4])
	}
	return data, nil
}

// DecodeChunk decrypts and decompresses a raw chunk body.
func DecodeChunk(raw, depotKey []byte) ([]byte, error) {
	plain, err := steamcrypto.SymmetricDecrypt(raw, depotKey)
	if err != nil {
		return nil, err
	}
	switch {
	case len(plain) >= 3 && plain[0] == 'V' && plain[1] == 'S' && plain[2] == 'Z':
		return decompressVZstd(plain)
	case len(plain) >= 2 && plain[0] == 'V' && plain[1] == 'Z':
		return decompressVZip(plain)
	case len(plain) >= 2 && plain[0] == 'P' && plain[1] == 'K':
		return decompressZip(plain)
	}
	return nil, errors.New("unknown chunk compression")
}

// VZip: "VZ", version byte, 4 bytes timestamp/crc, 5 LZMA property bytes,
// raw LZMA stream, then a 10 byte footer of crc32, size and "zv".
func decompressVZip(b []byte) ([]byte, error) {
	const header, footer, props = 7, 10, 5
	if len(b) < header+props+footer {
		return nil, errors.New("vzip: too short")
	}
	if b[2] != 'a' {
		return nil, fmt.Errorf("vzip: unsupported version %q", b[2])
	}
	if b[len(b)-2] != 'z' || b[len(b)-1] != 'v' {
		return nil, errors.New("vzip: bad footer")
	}
	size := binary.LittleEndian.Uint32(b[len(b)-6:])
	body := b[header : len(b)-footer]

	// The lzma package wants the classic 13 byte header: properties plus an
	// explicit uncompressed size, which the footer supplies.
	stream := make([]byte, 0, 13+len(body)-props)
	stream = append(stream, body[:props]...)
	stream = binary.LittleEndian.AppendUint64(stream, uint64(size))
	stream = append(stream, body[props:]...)

	r, err := lzma.NewReader(bytes.NewReader(stream))
	if err != nil {
		return nil, fmt.Errorf("vzip: %w", err)
	}
	out := make([]byte, size)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("vzip: %w", err)
	}
	return out, nil
}

// VZstd: "VSZa" then a zstd frame then a 12 byte footer of crc32, size
// and a trailing magic.
func decompressVZstd(b []byte) ([]byte, error) {
	const header, footer = 4, 12
	if len(b) < header+footer {
		return nil, errors.New("vzstd: too short")
	}
	size := binary.LittleEndian.Uint32(b[len(b)-8:])
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	out, err := dec.DecodeAll(b[header:len(b)-footer], make([]byte, 0, size))
	if err != nil {
		return nil, fmt.Errorf("vzstd: %w", err)
	}
	return out, nil
}

func decompressZip(b []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, fmt.Errorf("zip chunk: %w", err)
	}
	if len(zr.File) == 0 {
		return nil, errors.New("zip chunk: no entries")
	}
	f, err := zr.File[0].Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
