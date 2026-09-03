package depot

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/itchio/fresh-steamer/cdn"
	"github.com/itchio/fresh-steamer/steamcrypto"
)

var testKey = bytes.Repeat([]byte{0x11}, 32)

// fakeCDN serves encrypted zip-framed chunks by sha and counts requests.
type fakeCDN struct {
	chunks map[string][]byte
	hits   atomic.Int32
	srv    *httptest.Server
}

func newFakeCDN(t *testing.T) *fakeCDN {
	f := &fakeCDN{chunks: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		parts := strings.Split(r.URL.Path, "/")
		body, ok := f.chunks[parts[len(parts)-1]]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCDN) client() *cdn.Client {
	return cdn.NewClient([]cdn.Server{{Host: strings.TrimPrefix(f.srv.URL, "http://")}})
}

func (f *fakeCDN) chunk(t *testing.T, plain []byte, offset uint64) *cdn.Chunk {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fw, _ := zw.Create("z")
	fw.Write(plain)
	zw.Close()
	enc, err := steamcrypto.SymmetricEncrypt(buf.Bytes(), testKey, bytes.Repeat([]byte{2}, 16))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(plain)
	f.chunks[hex.EncodeToString(sum[:])] = enc
	return &cdn.Chunk{SHA: sum[:], Checksum: steamcrypto.Adler(plain), Offset: offset, Size: uint32(len(plain))}
}

func TestDownloadDedupesChunks(t *testing.T) {
	f := newFakeCDN(t)
	shared := bytes.Repeat([]byte("shared"), 100)
	unique := bytes.Repeat([]byte("unique"), 50)

	// a.bin is shared+unique, b.bin is shared+shared: three chunk slots but
	// only two distinct chunks.
	a := &cdn.File{Name: "dir/a.bin", Size: uint64(len(shared) + len(unique)), Chunks: []*cdn.Chunk{
		f.chunk(t, shared, 0), f.chunk(t, unique, uint64(len(shared))),
	}}
	b := &cdn.File{Name: "b.bin", Size: uint64(2 * len(shared)), Flags: cdn.FlagExecutable, Chunks: []*cdn.Chunk{
		f.chunk(t, shared, 0), f.chunk(t, shared, uint64(len(shared))),
	}}
	m := &cdn.Manifest{Files: []*cdn.File{
		{Name: "dir", Flags: cdn.FlagDirectory},
		a, b,
		{Name: "empty.txt"},
	}}

	dir := t.TempDir()
	var last Progress
	err := Download(context.Background(), f.client(), Options{
		Dir: dir, DepotID: 1, DepotKey: testKey, Manifest: m,
		OnProgress: func(p Progress) { last = p },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.hits.Load(); got != 2 {
		t.Fatalf("expected 2 chunk fetches, got %d", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "dir/a.bin")); !bytes.Equal(got, append(append([]byte{}, shared...), unique...)) {
		t.Fatal("a.bin content mismatch")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.bin")); !bytes.Equal(got, append(append([]byte{}, shared...), shared...)) {
		t.Fatal("b.bin content mismatch")
	}
	if st, _ := os.Stat(filepath.Join(dir, "b.bin")); st.Mode()&0o111 == 0 {
		t.Fatal("b.bin should be executable")
	}
	if st, _ := os.Stat(filepath.Join(dir, "empty.txt")); st.Size() != 0 {
		t.Fatal("empty.txt should exist and be empty")
	}
	if last.FilesDone != 4 || last.BytesDone != last.BytesTotal {
		t.Fatalf("progress: %+v", last)
	}
}

func TestDownloadSkipsUnchangedAndRemovesStale(t *testing.T) {
	f := newFakeCDN(t)
	data := bytes.Repeat([]byte("x"), 300)
	sum := sha1.Sum(data)
	file := &cdn.File{Name: "keep.bin", Size: 300, SHAContent: sum[:], Chunks: []*cdn.Chunk{f.chunk(t, data, 0)}}
	old := &cdn.Manifest{Files: []*cdn.File{file, {Name: "gone.bin", Size: 1}}}
	dir := t.TempDir()
	store := &Store{Dir: filepath.Join(dir, ".state")}

	if err := Download(context.Background(), f.client(), Options{Dir: dir, DepotID: 1, DepotKey: testKey, Manifest: old, Store: store}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "gone.bin"), []byte("z"), 0o644)
	f.hits.Store(0)

	next := &cdn.Manifest{Files: []*cdn.File{file}}
	var last Progress
	err := Download(context.Background(), f.client(), Options{Dir: dir, DepotID: 1, DepotKey: testKey, Manifest: next, Store: store,
		OnProgress: func(p Progress) { last = p }})
	if err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 0 {
		t.Fatalf("expected no fetches, got %d", f.hits.Load())
	}
	if last.BytesSkipped != 300 {
		t.Fatalf("skipped: %+v", last)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.bin")); !os.IsNotExist(err) {
		t.Fatal("stale file should be removed")
	}
	if prev, _ := store.Previous(1); prev == nil || len(prev.Files) != 1 {
		t.Fatal("store should hold the new manifest")
	}
}

func TestDownloadResumesAfterInterruption(t *testing.T) {
	f := newFakeCDN(t)
	var parts [][]byte
	var chunks []*cdn.Chunk
	var offset uint64
	for i := 0; i < 6; i++ {
		p := bytes.Repeat([]byte{byte('a' + i)}, 200)
		parts = append(parts, p)
		chunks = append(chunks, f.chunk(t, p, offset))
		offset += 200
	}
	file := &cdn.File{Name: "big.bin", Size: offset, Chunks: chunks}
	m := &cdn.Manifest{GID: 77, Files: []*cdn.File{file}}
	dir := t.TempDir()
	store := &Store{Dir: filepath.Join(dir, ".state")}

	// Fail every request after the third so the first run dies part way.
	var served atomic.Int32
	failing := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if served.Add(1) > 3 {
			w.WriteHeader(500)
			return
		}
		failing.ServeHTTP(w, r)
	})
	client := f.client()
	client.Retries = 1
	client.Backoff = 1
	opts := Options{Dir: dir, DepotID: 1, DepotKey: testKey, Manifest: m, Store: store, Concurrency: 1}
	if err := Download(context.Background(), client, opts); err == nil {
		t.Fatal("expected first run to fail")
	}
	j, err := store.Journal(1)
	if err != nil || j == nil || j.GID != 77 || len(j.Done) != 3 {
		t.Fatalf("journal after failure: %+v %v", j, err)
	}

	f.srv.Config.Handler = failing
	f.hits.Store(0)
	var last Progress
	opts.OnProgress = func(p Progress) { last = p }
	if err := Download(context.Background(), client, opts); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 3 {
		t.Fatalf("expected 3 fetches on resume, got %d", f.hits.Load())
	}
	if last.BytesSkipped != 600 {
		t.Fatalf("expected 600 bytes skipped, got %+v", last)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "big.bin"))
	if !bytes.Equal(got, bytes.Join(parts, nil)) {
		t.Fatal("content mismatch after resume")
	}
	if j, _ := store.Journal(1); j != nil {
		t.Fatal("journal should be cleared after success")
	}

	// A different manifest id must not resume from the old journal. The
	// bytes on disk are still reused, but only after hashing them, so a
	// corrupted chunk is fetched again.
	store.SaveJournal(1, &Journal{GID: 1, Done: j0(chunks)})
	fh, _ := os.OpenFile(filepath.Join(dir, "big.bin"), os.O_WRONLY, 0)
	fh.WriteAt([]byte("corrupt"), 450)
	fh.Close()
	f.hits.Store(0)
	m2 := &cdn.Manifest{GID: 78, Files: []*cdn.File{file}}
	if err := Download(context.Background(), client, Options{Dir: dir, DepotID: 1, DepotKey: testKey, Manifest: m2, Store: store, OnProgress: func(p Progress) { last = p }}); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 1 || last.BytesReused != 1000 {
		t.Fatalf("expected 1 fetch and 1000 bytes reused with stale journal, got %d fetches, %+v", f.hits.Load(), last)
	}
	got, _ = os.ReadFile(filepath.Join(dir, "big.bin"))
	if !bytes.Equal(got, bytes.Join(parts, nil)) {
		t.Fatal("content mismatch after reuse")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*"+oldSuffix)); len(leftovers) != 0 {
		t.Fatalf("stashed files left behind: %v", leftovers)
	}
}

func TestDownloadReusesChunksFromOldVersions(t *testing.T) {
	f := newFakeCDN(t)
	a := bytes.Repeat([]byte("A"), 300)
	b := bytes.Repeat([]byte("B"), 300)
	c := bytes.Repeat([]byte("C"), 300)
	x := bytes.Repeat([]byte("X"), 100)
	d := bytes.Repeat([]byte("D"), 300)

	// v1: pack.bin = A B C, old.bin = D
	v1 := &cdn.Manifest{GID: 1, Files: []*cdn.File{
		{Name: "pack.bin", Size: 900, Chunks: []*cdn.Chunk{f.chunk(t, a, 0), f.chunk(t, b, 300), f.chunk(t, c, 600)}},
		{Name: "old.bin", Size: 300, Chunks: []*cdn.Chunk{f.chunk(t, d, 0)}},
	}}
	dir := t.TempDir()
	store := &Store{Dir: filepath.Join(dir, ".state")}
	if err := Download(context.Background(), f.client(), Options{Dir: dir, DepotID: 1, DepotKey: testKey, Manifest: v1, Store: store}); err != nil {
		t.Fatal(err)
	}

	// v2: X inserted in the middle of pack.bin so B and C shift, and
	// old.bin renamed to new.bin. Only X is new.
	v2 := &cdn.Manifest{GID: 2, Files: []*cdn.File{
		{Name: "pack.bin", Size: 1000, Chunks: []*cdn.Chunk{f.chunk(t, a, 0), f.chunk(t, x, 300), f.chunk(t, b, 400), f.chunk(t, c, 700)}},
		{Name: "new.bin", Size: 300, Chunks: []*cdn.Chunk{f.chunk(t, d, 0)}},
	}}
	f.hits.Store(0)
	var last Progress
	if err := Download(context.Background(), f.client(), Options{Dir: dir, DepotID: 1, DepotKey: testKey, Manifest: v2, Store: store, OnProgress: func(p Progress) { last = p }}); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 1 || last.BytesReused != 1200 {
		t.Fatalf("expected 1 fetch and 1200 bytes reused, got %d fetches, %+v", f.hits.Load(), last)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "pack.bin"))
	if !bytes.Equal(got, bytes.Join([][]byte{a, x, b, c}, nil)) {
		t.Fatal("pack.bin content mismatch")
	}
	got, _ = os.ReadFile(filepath.Join(dir, "new.bin"))
	if !bytes.Equal(got, d) {
		t.Fatal("new.bin content mismatch")
	}
	if _, err := os.Stat(filepath.Join(dir, "old.bin")); !os.IsNotExist(err) {
		t.Fatal("old.bin should have been removed")
	}

	// No previous manifest at all: an existing file with A at offset 0
	// still contributes that chunk by the same-offset guess.
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir2, "pack.bin"), bytes.Join([][]byte{a, bytes.Repeat([]byte("?"), 700)}, nil), 0o644)
	f.hits.Store(0)
	if err := Download(context.Background(), f.client(), Options{Dir: dir2, DepotID: 1, DepotKey: testKey, Manifest: v2, OnProgress: func(p Progress) { last = p }}); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 4 || last.BytesReused != 300 {
		t.Fatalf("expected 4 fetches and 300 bytes reused without previous manifest, got %d fetches, %+v", f.hits.Load(), last)
	}
	got, _ = os.ReadFile(filepath.Join(dir2, "pack.bin"))
	if !bytes.Equal(got, bytes.Join([][]byte{a, x, b, c}, nil)) {
		t.Fatal("pack.bin content mismatch without previous")
	}
}

func j0(chunks []*cdn.Chunk) []string {
	var out []string
	for _, c := range chunks {
		out = append(out, hex.EncodeToString(c.SHA))
	}
	return out
}
