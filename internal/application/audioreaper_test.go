package application

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReapStaleAudioKeepsPendingAndRecentRecordings(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	write := func(name string, age time.Duration) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	stale := write("stale.webm", 2*AudioRetention)
	recent := write("recent.webm", time.Minute)
	pending := write("pending.webm", 2*AudioRetention)
	if _, err := f.app.CreateJob(context.Background(), f.user.ID, "voice_transcription", "audio", "", 0, map[string]any{"path": pending}); err != nil {
		t.Fatal(err)
	}
	removed, err := ReapStaleAudio(context.Background(), f.store, dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d, want 1", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale recording was kept")
	}
	for _, path := range []string{recent, pending} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was removed: %v", filepath.Base(path), err)
		}
	}
}
