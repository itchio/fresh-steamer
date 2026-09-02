package store

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Download fetches a URL into dir, naming the file after the URL path
// with the prefix in front. It returns the local path.
func Download(ctx context.Context, hc *http.Client, rawURL, dir, prefix string) (string, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	name := path.Base(u.Path)
	if name == "" || name == "/" || name == "." {
		name = "asset"
	}
	if prefix != "" {
		name = prefix + "_" + name
	}
	// Steam appends a cache-busting query; the path base is stable.
	dest := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	res, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", rawURL, err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("downloading %s: HTTP %d", rawURL, res.StatusCode)
	}
	if !strings.Contains(name, ".") {
		if ext := extFromType(res.Header.Get("Content-Type")); ext != "" {
			dest += ext
		}
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, res.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return dest, os.Rename(tmp, dest)
}

func extFromType(ct string) string {
	switch {
	case strings.HasPrefix(ct, "image/jpeg"):
		return ".jpg"
	case strings.HasPrefix(ct, "image/png"):
		return ".png"
	case strings.HasPrefix(ct, "image/webp"):
		return ".webp"
	case strings.HasPrefix(ct, "image/gif"):
		return ".gif"
	}
	return ""
}
