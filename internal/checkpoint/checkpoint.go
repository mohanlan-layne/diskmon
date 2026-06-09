// Package checkpoint persists and loads per-volume USN Journal progress.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Data is the checkpoint state for a single volume.
type Data struct {
	JournalID uint64    `json:"journalId"`
	NextUSN   int64     `json:"nextUsn"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Load reads the checkpoint for the given volume from dir.
// Returns an empty Data (all zeros) if no checkpoint file exists yet.
func Load(dir, volume string) (Data, error) {
	path := filePath(dir, volume)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Data{}, nil
	}
	if err != nil {
		return Data{}, fmt.Errorf("read checkpoint %s: %w", path, err)
	}
	var d Data
	if err := json.Unmarshal(data, &d); err != nil {
		return Data{}, fmt.Errorf("parse checkpoint %s: %w", path, err)
	}
	return d, nil
}

// Save writes the checkpoint for the given volume atomically (write + rename).
func Save(dir, volume string, d Data) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir checkpoint dir %s: %w", dir, err)
	}
	d.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}

	path := filePath(dir, volume)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write checkpoint tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename checkpoint %s → %s: %w", tmp, path, err)
	}
	return nil
}

// filePath returns the checkpoint file path for the given volume.
// Volume special characters (: \ /) are stripped to produce a safe filename.
func filePath(dir, volume string) string {
	safe := strings.NewReplacer(":", "", "\\", "", "/", "").Replace(volume)
	return filepath.Join(dir, "checkpoint-"+strings.ToLower(safe)+".json")
}
