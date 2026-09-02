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
	var todo []*cdn.File
	for _, f := range opts.Manifest.Files {
		if err := checkName(f.Name); err != nil {
			return err
		}
		delete(prev, f.Name)
		prog.FilesTotal++
		if f.IsDir() || f.IsSymlink() {
			continue
		}
		prog.BytesTotal += f.Size
		todo = append(todo, f)
	}
	report := func() {
		if opts.OnProgress != nil {
			opts.OnProgress(prog)
		}
	}

	for name := range prev {
		p := filepath.Join(opts.Dir, filepath.FromSlash(name))
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("removing stale %s: %w", name, err)
		}
	}

	for _, f := range opts.Manifest.Files {
		p := filepath.Join(opts.Dir, filepath.FromSlash(f.Name))
		switch {
		case f.IsDir():
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			mu.Lock()
			prog.FilesDone++
			mu.Unlock()
		case f.IsSymlink():
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			os.Remove(p)
			if err := os.Symlink(filepath.FromSlash(f.LinkTarget), p); err != nil {
				return fmt.Errorf("creating symlink %s: %w", f.Name, err)
			}
			mu.Lock()
			prog.FilesDone++
			mu.Unlock()
		}
	}
	report()

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(opts.Concurrency)
	for _, f := range todo {
		f := f
		p := filepath.Join(opts.Dir, filepath.FromSlash(f.Name))
		if old, ok := unchanged(opts.Previous, f); ok {
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && uint64(st.Size()) == old.Size {
				mu.Lock()
				prog.FilesDone++
				prog.BytesDone += f.Size
				prog.BytesSkipped += f.Size
				mu.Unlock()
				report()
				continue
			}
		}
		g.Go(func() error {
			if err := writeFile(ctx, c, opts, f, p, func(n uint64) {
				mu.Lock()
				prog.BytesDone += n
				mu.Unlock()
				report()
			}); err != nil {
				return err
			}
			mu.Lock()
			prog.FilesDone++
			mu.Unlock()
			report()
			return nil
		})
	}
	return g.Wait()
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

func writeFile(ctx context.Context, c *cdn.Client, opts Options, f *cdn.File, path string, advance func(uint64)) error {
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
	for _, ch := range f.Chunks {
		data, err := c.FetchChunk(ctx, opts.DepotID, ch, opts.DepotKey)
		if err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		if _, err := fh.WriteAt(data, int64(ch.Offset)); err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		advance(uint64(len(data)))
	}
	return nil
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
