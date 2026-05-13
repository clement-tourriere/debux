package history

import (
	"testing"
	"time"
)

func TestAppendLoadAndCapHistory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	for i := 0; i < maxEntries+5; i++ {
		if err := Append(Entry{StartedAt: time.Unix(int64(i), 0), Target: "docker://app"}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	entries, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(entries) != maxEntries {
		t.Fatalf("len(entries) = %d, want %d", len(entries), maxEntries)
	}
	if got := entries[0].StartedAt.Unix(); got != int64(maxEntries+4) {
		t.Fatalf("newest entry time = %d", got)
	}
}
