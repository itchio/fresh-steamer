// Package depot writes a manifest's files to disk, fetching chunks in
// parallel and skipping files unchanged since a previous manifest.
package depot

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/itchio/fresh-steamer/cdn"
	"golang.org/x/sync/errgroup"
)

type Progress struct {
	FilesTotal int
	FilesDone  int
	BytesTotal uint64
	BytesDone  uint64
	// BytesSkipped were left as they were on disk.
	BytesSkipped uint64
	// BytesReused were copied out of previous versions of files instead
	// of fetched.
	BytesReused uint64
}

type Options struct {
	Dir      string
	DepotID  uint32
	DepotKey []byte
	Manifest *cdn.Manifest
	// Previous, when set, lets files with an unchanged content hash be
	// skipped as long as they are still on disk at the right size.
	Previous *cdn.Manifest
	// Store supplies Previous when it is nil and records Manifest once the
	// download succeeds.
	Store       *Store
	Concurrency int
	OnProgress  func(Progress)
	Logf        func(format string, args ...any)
}

// Download materializes opts.Manifest under opts.Dir. Files present in
// Previous but absent from Manifest are removed.
//
// Steam builds are content addressed: each file is a list of chunks named
// by the SHA1 of their contents. Game engines pack assets into a few big
// archives, so a small change usually leaves most chunks of a file in
// place. When Previous is known, a changed file is rebuilt from the chunks
// its old version still holds, and only chunks not found locally hit the
// CDN.
func Download(ctx context.Context, c *cdn.Client, opts Options) error {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.Store != nil && opts.Previous == nil {
		prev, err := opts.Store.Previous(opts.DepotID)
		if err != nil {
			return err
		}
		opts.Previous = prev
	}
	var resume *Journal
	if opts.Store != nil {
		j, err := opts.Store.Journal(opts.DepotID)
		if err != nil {
			return err
		}
		if j != nil && j.GID == opts.Manifest.GID {
			resume = j
		}
	}
	if err := download(ctx, c, opts, resume); err != nil {
		return err
	}
	if opts.Store != nil {
		if err := opts.Store.ClearJournal(opts.DepotID); err != nil {
			return err
		}
		return opts.Store.Save(opts.DepotID, opts.Manifest)
	}
	return nil
}

func download(ctx context.Context, c *cdn.Client, opts Options, resume *Journal) error {
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return err
	}

	prev := map[string]*cdn.File{}
	prevByName := map[string]*cdn.File{}
	if opts.Previous != nil {
		for _, f := range opts.Previous.Files {
			prev[f.Name] = f
			prevByName[f.Name] = f
		}
	}

	var mu sync.Mutex
	prog := Progress{}
	report := func() {
		if opts.OnProgress != nil {
			opts.OnProgress(prog)
		}
	}

	for _, f := range opts.Manifest.Files {
		if err := checkName(f.Name); err != nil {
			return err
		}
		delete(prev, f.Name)
		prog.FilesTotal++
		if !f.IsDir() && !f.IsSymlink() {
			prog.BytesTotal += f.Size
		}
	}
	// Old versions of files being rewritten, moved aside so they can be
	// read while the new version is written over their path. Files gone
	// from the manifest go there too: a rename on Steam's side is a
	// stale file plus a new one with the same chunks.
	oldFiles := map[*cdn.File]string{}
	stashedByName := map[string]string{}

	for name, old := range prev {
		p := filepath.Join(opts.Dir, filepath.FromSlash(name))
		if !old.IsDir() && !old.IsSymlink() {
			if stashed := stashOld(p, old.Size); stashed != "" {
				oldFiles[old] = stashed
				continue
			}
		}
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("removing stale %s: %w", name, err)
		}
	}

	// Same chunk contents can appear in several files or several times in
	// one file. Fetch each distinct chunk once and write it everywhere.
	type target struct {
		path   string
		offset uint64
		file   *cdn.File
	}
	type work struct {
		chunk   *cdn.Chunk
		targets []target
	}
	byChunk := map[string]*work{}
	var order []*work
	pending := map[*cdn.File]int{}

	// A resumed chunk counts only if every file it lands in was left as the
	// interrupted run made it. Files that are missing or the wrong size get
	// all their chunks fetched again.
	done := map[string]bool{}
	if resume != nil {
		for _, h := range resume.Done {
			done[h] = true
		}
	}
	fresh := map[*cdn.File]bool{}

	for _, f := range opts.Manifest.Files {
		p := filepath.Join(opts.Dir, filepath.FromSlash(f.Name))
		switch {
		case f.IsDir():
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			prog.FilesDone++
			continue
		case f.IsSymlink():
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			os.Remove(p)
			if err := os.Symlink(filepath.FromSlash(f.LinkTarget), p); err != nil {
				return fmt.Errorf("creating symlink %s: %w", f.Name, err)
			}
			prog.FilesDone++
			continue
		}

		old := prevByName[f.Name]
		if old != nil && sameContent(old, f) {
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && uint64(st.Size()) == old.Size {
				prog.FilesDone++
				prog.BytesDone += f.Size
				prog.BytesSkipped += f.Size
				continue
			}
		}
		if old != nil {
			if stashed := stashOld(p, old.Size); stashed != "" {
				oldFiles[old] = stashed
				stashedByName[f.Name] = stashed
			}
		} else if resume == nil {
			// Bytes on disk with no known previous version are leftover
			// from something else. Chunks at the same offsets may match.
			if stashed := stashOld(p, 0); stashed != "" {
				oldFiles[&cdn.File{Name: f.Name}] = stashed
				stashedByName[f.Name] = stashed
			}
		}

		if resume != nil {
			st, err := os.Stat(p)
			fresh[f] = err != nil || !st.Mode().IsRegular() || uint64(st.Size()) != f.Size
		}
		if err := createFile(p, f); err != nil {
			return err
		}
		if len(f.Chunks) == 0 {
			prog.FilesDone++
			continue
		}
		pending[f] = len(f.Chunks)
		for _, ch := range f.Chunks {
			key := string(ch.SHA)
			w := byChunk[key]
			if w == nil {
				w = &work{chunk: ch}
				byChunk[key] = w
				order = append(order, w)
			}
			w.targets = append(w.targets, target{path: p, offset: ch.Offset, file: f})
		}
	}
	local := localChunks(opts, oldFiles, stashedByName)

	journal := &Journal{GID: opts.Manifest.GID}
	var todo []*work
	finish := func(w *work, n uint64, reused bool) {
		journal.Done = append(journal.Done, hex.EncodeToString(w.chunk.SHA))
		for _, t := range w.targets {
			prog.BytesDone += n
			if reused {
				prog.BytesReused += n
			} else {
				prog.BytesSkipped += n
			}
			pending[t.file]--
			if pending[t.file] == 0 {
				prog.FilesDone++
			}
		}
	}
	for _, w := range order {
		skip := done[hex.EncodeToString(w.chunk.SHA)]
		for _, t := range w.targets {
			if fresh[t.file] {
				skip = false
			}
		}
		if skip {
			finish(w, uint64(w.chunk.Size), false)
			continue
		}
		if src, ok := local[string(w.chunk.SHA)]; ok {
			data, err := src.read(w.chunk)
			if err == nil {
				for _, t := range w.targets {
					if err = writeAt(t.path, data, t.offset); err != nil {
						return fmt.Errorf("%s: %w", t.file.Name, err)
					}
				}
				finish(w, uint64(len(data)), true)
				continue
			}
			opts.Logf("depot: chunk %x from %s: %v, fetching instead", w.chunk.SHA, src.path, err)
		}
		todo = append(todo, w)
	}
	report()

	// The journal is flushed on a timer rather than per chunk so a big
	// depot does not turn into thousands of tiny writes.
	var journalMu sync.Mutex
	flush := func() {
		if opts.Store == nil {
			return
		}
		journalMu.Lock()
		snapshot := &Journal{GID: journal.GID, Done: append([]string(nil), journal.Done...)}
		journalMu.Unlock()
		if err := opts.Store.SaveJournal(opts.DepotID, snapshot); err != nil {
			opts.Logf("depot: saving progress journal: %v", err)
		}
	}
	stopFlush := make(chan struct{})
	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopFlush:
				return
			case <-t.C:
				flush()
			}
		}
	}()

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(opts.Concurrency)
	for _, w := range todo {
		w := w
		g.Go(func() error {
			data, err := c.FetchChunk(ctx, opts.DepotID, w.chunk, opts.DepotKey)
			if err != nil {
				return fmt.Errorf("%s: %w", w.targets[0].file.Name, err)
			}
			for _, t := range w.targets {
				if err := writeAt(t.path, data, t.offset); err != nil {
					return fmt.Errorf("%s: %w", t.file.Name, err)
				}
			}
			journalMu.Lock()
			journal.Done = append(journal.Done, hex.EncodeToString(w.chunk.SHA))
			journalMu.Unlock()
			mu.Lock()
			for _, t := range w.targets {
				prog.BytesDone += uint64(len(data))
				pending[t.file]--
				if pending[t.file] == 0 {
					prog.FilesDone++
				}
			}
			mu.Unlock()
			report()
			return nil
		})
	}
	err := g.Wait()
	close(stopFlush)
	<-flushDone
	if err != nil {
		// Leave the journal and old files behind so the next run resumes.
		flush()
		return err
	}
	for _, p := range oldFiles {
		os.Remove(p)
	}
	return removeStashes(opts.Dir)
}

// removeStashes sweeps stashes an interrupted run may have left under a
// manifest that is no longer the previous one.
func removeStashes(dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), oldSuffix) {
			return os.Remove(path)
		}
		return nil
	})
}

const oldSuffix = ".fresh-steamer-old"

// stashOld moves the previous version of a changed file out of the way
// and returns its new path, or "" when there is nothing usable on disk.
// A stash left by an interrupted run is kept as is. size 0 accepts any
// non-empty file.
func stashOld(p string, size uint64) string {
	oldP := p + oldSuffix
	if st, err := os.Stat(oldP); err == nil && st.Mode().IsRegular() {
		return oldP
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return ""
	}
	if size != 0 && uint64(st.Size()) != size {
		return ""
	}
	if err := os.Rename(p, oldP); err != nil {
		return ""
	}
	return oldP
}

type localChunk struct {
	path   string
	offset uint64
	size   uint32
}

// read returns the chunk's bytes from the local file, or an error when
// they no longer hash to what the manifest expects.
func (l localChunk) read(ch *cdn.Chunk) ([]byte, error) {
	fh, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	buf := make([]byte, l.size)
	if _, err := fh.ReadAt(buf, int64(l.offset)); err != nil {
		return nil, err
	}
	if sum := sha1.Sum(buf); !bytes.Equal(sum[:], ch.SHA) {
		return nil, fmt.Errorf("content changed")
	}
	return buf, nil
}

// localChunks indexes by SHA every chunk that may already be on disk.
// The previous manifest gives exact positions, in stashed old versions of
// rewritten files and in unchanged files still at their path. For files
// whose old layout is unknown, the guess is that a chunk sits at the same
// offset it has in the new manifest, which holds for appends and in-place
// edits. Every candidate is verified by hash before use.
func localChunks(opts Options, oldFiles map[*cdn.File]string, stashedByName map[string]string) map[string]localChunk {
	index := map[string]localChunk{}
	add := func(sha []byte, path string, offset uint64, size uint32) {
		if _, ok := index[string(sha)]; !ok {
			index[string(sha)] = localChunk{path: path, offset: offset, size: size}
		}
	}
	if opts.Previous != nil {
		for _, old := range opts.Previous.Files {
			if old.IsDir() || old.IsSymlink() {
				continue
			}
			path, stashed := oldFiles[old]
			if !stashed {
				path = filepath.Join(opts.Dir, filepath.FromSlash(old.Name))
				st, err := os.Stat(path)
				if err != nil || !st.Mode().IsRegular() || uint64(st.Size()) != old.Size {
					continue
				}
			}
			for _, ch := range old.Chunks {
				add(ch.SHA, path, ch.Offset, ch.Size)
			}
		}
	}
	for _, f := range opts.Manifest.Files {
		path, ok := stashedByName[f.Name]
		if !ok {
			continue
		}
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		for _, ch := range f.Chunks {
			if ch.Offset+uint64(ch.Size) <= uint64(st.Size()) {
				add(ch.SHA, path, ch.Offset, ch.Size)
			}
		}
	}
	return index
}

func createFile(path string, f *cdn.File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if f.IsExecutable() {
		mode = 0o755
	}
	// No O_TRUNC: on resume the existing bytes are what we are keeping, and
	// otherwise every byte gets rewritten by a chunk anyway.
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer fh.Close()
	if err := fh.Truncate(int64(f.Size)); err != nil {
		return err
	}
	if err := fh.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

func writeAt(path string, data []byte, offset uint64) error {
	fh, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer fh.Close()
	_, err = fh.WriteAt(data, int64(offset))
	return err
}

func sameContent(old, f *cdn.File) bool {
	return len(f.SHAContent) > 0 && bytes.Equal(old.SHAContent, f.SHAContent) && old.Size == f.Size
}

// checkName rejects paths that would escape the output directory.
func checkName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, ":") {
		return fmt.Errorf("refusing manifest path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return fmt.Errorf("refusing manifest path %q", name)
		}
	}
	return nil
}
