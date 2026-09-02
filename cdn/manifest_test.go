package cdn

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"testing"

	"github.com/itchio/fresh-steamer/pb"
	"github.com/itchio/fresh-steamer/steamcrypto"
	"google.golang.org/protobuf/proto"
)

func encryptName(t *testing.T, name string) string {
	enc, err := steamcrypto.SymmetricEncrypt([]byte(name), testKey, bytes.Repeat([]byte{5}, 16))
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(enc)
}

func buildManifest(t *testing.T, payload *pb.ContentManifestPayload, meta *pb.ContentManifestMetadata) []byte {
	var body []byte
	section := func(magic uint32, m proto.Message) {
		raw, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		body = binary.LittleEndian.AppendUint32(body, magic)
		body = binary.LittleEndian.AppendUint32(body, uint32(len(raw)))
		body = append(body, raw...)
	}
	section(magicPayload, payload)
	section(magicMetadata, meta)
	section(magicSignature, &pb.ContentManifestSignature{Signature: []byte("sig")})
	body = binary.LittleEndian.AppendUint32(body, magicEnd)

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("z")
	f.Write(body)
	w.Close()
	return buf.Bytes()
}

func TestParseManifestEncryptedNames(t *testing.T) {
	payload := &pb.ContentManifestPayload{
		Mappings: []*pb.ContentManifestPayload_FileMapping{
			{
				Filename: proto.String(encryptName(t, "bin\\game.exe")),
				Size:     proto.Uint64(10),
				Flags:    proto.Uint32(FlagExecutable),
				Chunks: []*pb.ContentManifestPayload_FileMapping_ChunkData{
					{Sha: []byte("01234567890123456789"), Crc: proto.Uint32(7), Offset: proto.Uint64(0), CbOriginal: proto.Uint32(10), CbCompressed: proto.Uint32(12)},
				},
			},
			{
				Filename:   proto.String(encryptName(t, "link")),
				Flags:      proto.Uint32(FlagSymlink),
				Linktarget: proto.String(encryptName(t, "bin\\game.exe")),
			},
			{
				Filename: proto.String(encryptName(t, "bin")),
				Flags:    proto.Uint32(FlagDirectory),
			},
		},
	}
	meta := &pb.ContentManifestMetadata{
		DepotId:            proto.Uint32(441),
		GidManifest:        proto.Uint64(99),
		FilenamesEncrypted: proto.Bool(true),
		CbDiskOriginal:     proto.Uint64(10),
	}
	raw := buildManifest(t, payload, meta)

	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !m.FilenamesEncrypted || m.DepotID != 441 || m.GID != 99 {
		t.Fatalf("metadata: %+v", m)
	}
	if err := m.decryptFilenames(testKey); err != nil {
		t.Fatal(err)
	}
	if m.Files[0].Name != "bin/game.exe" || !m.Files[0].IsExecutable() {
		t.Fatalf("file 0: %+v", m.Files[0])
	}
	if c := m.Files[0].Chunks[0]; c.Checksum != 7 || c.Size != 10 || c.Compressed != 12 {
		t.Fatalf("chunk: %+v", c)
	}
	if m.Files[1].Name != "link" || !m.Files[1].IsSymlink() || m.Files[1].LinkTarget != "bin/game.exe" {
		t.Fatalf("file 1: %+v", m.Files[1])
	}
	if !m.Files[2].IsDir() {
		t.Fatalf("file 2: %+v", m.Files[2])
	}
}

func TestParseManifestRejectsTruncated(t *testing.T) {
	raw := buildManifest(t, &pb.ContentManifestPayload{}, &pb.ContentManifestMetadata{})
	if _, err := ParseManifest(raw[:len(raw)/2]); err == nil {
		t.Fatal("expected error")
	}
	if _, err := ParseManifest([]byte("nope")); err == nil {
		t.Fatal("expected error")
	}
}
