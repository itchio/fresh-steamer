// Package depot writes a manifest's files to disk, fetching chunks in
// parallel and skipping files unchanged since a previous manifest.
package depot

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/itchio/fresh-steamer/cdn"
	"golang.org/x/sync/errgroup"
)

type Progress struct {
	FilesTotal   int
	FilesDone    int
	BytesTotal   uint64
	BytesDone    uint64
	BytesSkipped uint64
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
	if err := download(ctx, c, opts); err != nil {
		return err
	}
	if opts.Store != nil {
		return opts.Store.Save(opts.DepotID, opts.Manifest)
	}
	return nil
}

func download(ctx context.Context, c *cdn.Client, opts Options) error {
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return err
	}

	prev := map[string]*cdn.File{}
	if opts.Previous != nil {
		for _, f := range opts.Previous.Files {
			prev[f.Name] = f
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
	for name := range prev {
		p := filepath.Join(opts.Dir, filepath.FromSlash(name))
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

		if old, ok := unchanged(opts.Previous, f); ok {
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && uint64(st.Size()) == old.Size {
				prog.FilesDone++
				prog.BytesDone += f.Size
				prog.BytesSkipped += f.Size
				continue
			}
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
	report()

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(opts.Concurrency)
	for _, w := range order {
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
	return g.Wait()
}

func createFile(path string, f *cdn.File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if f.IsExecutable() {
		mode = 0o755
	}
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
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

func unchanged(previous *cdn.Manifest, f *cdn.File) (*cdn.File, bool) {
	if previous == nil || len(f.SHAContent) == 0 {
		return nil, false
	}
	for _, old := range previous.Files {
		if old.Name == f.Name {
			return old, bytes.Equal(old.SHAContent, f.SHAContent) && old.Size == f.Size
		}
	}
	return nil, false
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
