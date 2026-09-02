package cdn

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/itchio/fresh-steamer/pb"
	"github.com/itchio/fresh-steamer/steamcrypto"
	"google.golang.org/protobuf/proto"
)

// Magic values framing the sections inside a manifest zip entry.
const (
	magicPayload   = 0x71F617D0
	magicMetadata  = 0x1F4812BE
	magicSignature = 0x1B81B817
	magicEnd       = 0x32C415AB
)

// File flags from EDepotFileFlag.
const (
	FlagUserConfig       = 1 << 0
	FlagVersionedConfig  = 1 << 1
	FlagEncrypted        = 1 << 2
	FlagReadOnly         = 1 << 3
	FlagHidden           = 1 << 4
	FlagExecutable       = 1 << 5
	FlagDirectory        = 1 << 6
	FlagCustomExecutable = 1 << 7
	FlagInstallScript    = 1 << 8
	FlagSymlink          = 1 << 9
)

type Manifest struct {
	DepotID            uint32
	GID                uint64
	CreationTime       uint32
	TotalSize          uint64
	TotalCompressed    uint64
	UniqueChunks       uint32
	FilenamesEncrypted bool
	Files              []*File
}

type File struct {
	// Name uses forward slashes regardless of platform.
	Name       string
	Size       uint64
	Flags      uint32
	SHAContent []byte
	LinkTarget string
	Chunks     []*Chunk
}

func (f *File) IsDir() bool     { return f.Flags&FlagDirectory != 0 }
func (f *File) IsSymlink() bool { return f.Flags&FlagSymlink != 0 }
func (f *File) IsExecutable() bool {
	return f.Flags&(FlagExecutable|FlagCustomExecutable) != 0
}

type Chunk struct {
	SHA        []byte
	Checksum   uint32
	Offset     uint64
	Size       uint32
	Compressed uint32
}

// FetchManifest downloads and parses a manifest. depotKey may be nil when
// filenames are not encrypted, which is never the case for modern depots.
func (c *Client) FetchManifest(ctx context.Context, depotID uint32, gid uint64, requestCode uint64, depotKey []byte) (*Manifest, error) {
	path := fmt.Sprintf("/depot/%d/manifest/%d/5", depotID, gid)
	if requestCode != 0 {
		path = fmt.Sprintf("%s/%d", path, requestCode)
	}
	var m *Manifest
	err := c.retry(ctx, fmt.Sprintf("manifest %d", gid), func() error {
		raw, err := c.get(ctx, path)
		if err != nil {
			return err
		}
		m, err = ParseManifest(raw)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("fetching manifest %d for depot %d: %w", gid, depotID, err)
	}
	if m.FilenamesEncrypted {
		if depotKey == nil {
			return nil, errors.New("manifest has encrypted filenames but no depot key given")
		}
		if err := m.decryptFilenames(depotKey); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// ParseManifest decodes the zip-wrapped protobuf manifest format.
func ParseManifest(raw []byte) (*Manifest, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("manifest is not a zip: %w", err)
	}
	if len(zr.File) == 0 {
		return nil, errors.New("manifest zip is empty")
	}
	f, err := zr.File[0].Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	var payload pb.ContentManifestPayload
	var meta pb.ContentManifestMetadata
	havePayload, haveMeta := false, false
	for len(data) >= 4 {
		magic := binary.LittleEndian.Uint32(data)
		data = data[4:]
		if magic == magicEnd {
			break
		}
		if len(data) < 4 {
			return nil, errors.New("truncated manifest section")
		}
		n := binary.LittleEndian.Uint32(data)
		data = data[4:]
		if uint32(len(data)) < n {
			return nil, errors.New("truncated manifest section")
		}
		section := data[:n]
		data = data[n:]
		switch magic {
		case magicPayload:
			if err := proto.Unmarshal(section, &payload); err != nil {
				return nil, fmt.Errorf("manifest payload: %w", err)
			}
			havePayload = true
		case magicMetadata:
			if err := proto.Unmarshal(section, &meta); err != nil {
				return nil, fmt.Errorf("manifest metadata: %w", err)
			}
			haveMeta = true
		case magicSignature:
		default:
			return nil, fmt.Errorf("unknown manifest section magic %08x", magic)
		}
	}
	if !havePayload || !haveMeta {
		return nil, errors.New("manifest missing payload or metadata")
	}

	m := &Manifest{
		DepotID:            meta.GetDepotId(),
		GID:                meta.GetGidManifest(),
		CreationTime:       meta.GetCreationTime(),
		TotalSize:          meta.GetCbDiskOriginal(),
		TotalCompressed:    meta.GetCbDiskCompressed(),
		UniqueChunks:       meta.GetUniqueChunks(),
		FilenamesEncrypted: meta.GetFilenamesEncrypted(),
	}
	for _, fm := range payload.GetMappings() {
		file := &File{
			Name:       fm.GetFilename(),
			Size:       fm.GetSize(),
			Flags:      fm.GetFlags(),
			SHAContent: fm.GetShaContent(),
			LinkTarget: fm.GetLinktarget(),
		}
		for _, ch := range fm.GetChunks() {
			file.Chunks = append(file.Chunks, &Chunk{
				SHA:        ch.GetSha(),
				Checksum:   ch.GetCrc(),
				Offset:     ch.GetOffset(),
				Size:       ch.GetCbOriginal(),
				Compressed: ch.GetCbCompressed(),
			})
		}
		m.Files = append(m.Files, file)
	}
	if !m.FilenamesEncrypted {
		m.normalizeNames()
	}
	return m, nil
}

func (m *Manifest) decryptFilenames(key []byte) error {
	for _, f := range m.Files {
		name, err := decryptName(f.Name, key)
		if err != nil {
			return fmt.Errorf("decrypting filename: %w", err)
		}
		f.Name = name
		if f.LinkTarget != "" {
			target, err := decryptName(f.LinkTarget, key)
			if err != nil {
				return fmt.Errorf("decrypting link target: %w", err)
			}
			f.LinkTarget = target
		}
	}
	m.FilenamesEncrypted = false
	m.normalizeNames()
	return nil
}

func decryptName(enc string, key []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	plain, err := steamcrypto.SymmetricDecrypt(raw, key)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(plain), "\x00"), nil
}

// Manifests store Windows-style separators.
func (m *Manifest) normalizeNames() {
	for _, f := range m.Files {
		f.Name = strings.ReplaceAll(f.Name, "\\", "/")
		f.LinkTarget = strings.ReplaceAll(f.LinkTarget, "\\", "/")
	}
}

// get tries every server once, starting from a rotating offset.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	if len(c.Servers) == 0 {
		return nil, errors.New("cdn: no servers configured")
	}
	start := int(c.next.Add(1)-1) % len(c.Servers)
	var lastErr error
	for i := range c.Servers {
		srv := c.Servers[(start+i)%len(c.Servers)]
		req, err := http.NewRequestWithContext(ctx, "GET", srv.BaseURL()+path, nil)
		if err != nil {
			return nil, err
		}
		if srv.VHost != "" {
			req.Host = srv.VHost
		}
		req.Header.Set("User-Agent", "fresh-steamer/0.1")
		res, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if res.StatusCode == 200 {
			return body, nil
		}
		lastErr = fmt.Errorf("%s: HTTP %d", srv.Host, res.StatusCode)
		if res.StatusCode == 401 || res.StatusCode == 403 || res.StatusCode == 404 {
			return nil, permanentError{lastErr}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}
