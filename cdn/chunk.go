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
	path := fmt.Sprintf("/depot/%d/chunk/%s", depotID, hex.EncodeToString(chunk.SHA))
	var data []byte
	err := c.retry(ctx, fmt.Sprintf("chunk %x", chunk.SHA[:4]), func() error {
		raw, err := c.get(ctx, path)
		if err != nil {
			return err
		}
		// Decode and checksum failures are retried too: a bad body from one
		// cache node is the most likely cause.
		data, err = DecodeChunk(raw, depotKey)
		if err != nil {
			return fmt.Errorf("decoding: %w", err)
		}
		c.Logf("cdn: chunk %x %s %d -> %d bytes", chunk.SHA[:4], compressionName(raw, depotKey), len(raw), len(data))
		if uint32(len(data)) != chunk.Size {
			return fmt.Errorf("got %d bytes, manifest says %d", len(data), chunk.Size)
		}
		if sum := steamcrypto.Adler(data); sum != chunk.Checksum {
			return errors.New("checksum mismatch")
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("chunk %x: %w", chunk.SHA[:4], err)
	}
	return data, nil
}

func compressionName(raw, key []byte) string {
	plain, err := steamcrypto.SymmetricDecrypt(raw, key)
	if err != nil || len(plain) < 3 {
		return "?"
	}
	switch {
	case plain[0] == 'V' && plain[1] == 'S' && plain[2] == 'Z':
		return "vzstd"
	case plain[0] == 'V' && plain[1] == 'Z':
		return "vzip"
	case plain[0] == 'P' && plain[1] == 'K':
		return "zip"
	}
	return "?"
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

// VZstd: "VSZa" and a crc32 make an 8 byte header, then a zstd frame, then
// a 15 byte footer of crc32, uncompressed size, 4 zero bytes and "zsv".
func decompressVZstd(b []byte) ([]byte, error) {
	const header, footer = 8, 15
	if len(b) < header+footer {
		return nil, errors.New("vzstd: too short")
	}
	if b[3] != 'a' {
		return nil, fmt.Errorf("vzstd: unsupported version %q", b[3])
	}
	if !bytes.HasSuffix(b, []byte("zsv")) {
		return nil, errors.New("vzstd: bad footer")
	}
	size := binary.LittleEndian.Uint32(b[len(b)-11:])
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	out, err := dec.DecodeAll(b[header:len(b)-footer], make([]byte, 0, size))
	if err != nil {
		return nil, fmt.Errorf("vzstd: %w", err)
	}
	if uint32(len(out)) != size {
		return nil, fmt.Errorf("vzstd: footer says %d bytes, got %d", size, len(out))
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
