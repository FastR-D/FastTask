package application

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
)

// AudioRetention is how long a recording may outlive the job that needed it. The worker deletes the
// file when transcription succeeds; a failed or cancelled job keeps it this long so a retry can still
// read it, and then it goes. A voice note is the user's own speech and must not pile up on disk.
const AudioRetention = 24 * time.Hour

// ReapStaleAudio removes recordings in dir that are older than AudioRetention and are not the input of
// a queued or running transcription job. It returns how many files it removed.
func ReapStaleAudio(ctx context.Context, store *persistence.Store, dir string, now time.Time) (int, error) {
	if dir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var pending []persistence.AgentJob
	if err := store.DB.WithContext(ctx).Select("input_json").Where("type = 'voice_transcription' AND status IN ('queued','running')").Find(&pending).Error; err != nil {
		return 0, err
	}
	inUse := map[string]bool{}
	for _, job := range pending {
		var input struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(job.InputJSON), &input) == nil && input.Path != "" {
			inUse[filepath.Clean(input.Path)] = true
		}
	}
	removed := 0
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < AudioRetention {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if inUse[filepath.Clean(path)] {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	return removed, nil
}
