package depot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/itchio/fresh-steamer/cdn"
)

// Store remembers the last manifest written for each depot so a later
// Download can skip files that have not changed.
type Store struct {
	Dir string
}

func (s *Store) path(depotID uint32) string {
	return filepath.Join(s.Dir, fmt.Sprintf("manifest-%d.json", depotID))
}

// Previous returns the last saved manifest for the depot, or nil if none.
func (s *Store) Previous(depotID uint32) (*cdn.Manifest, error) {
	data, err := os.ReadFile(s.path(depotID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m cdn.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		// A corrupt record just means a full download next time.
		return nil, nil
	}
	return &m, nil
}

func (s *Store) Save(depotID uint32, m *cdn.Manifest) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := s.path(depotID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(depotID))
}

func (s *Store) Forget(depotID uint32) error {
	err := os.Remove(s.path(depotID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
