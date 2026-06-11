package history

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendRecoversFromCorruptHistory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{torn-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A corrupt file must not disable history forever.
	if err := Append(Entry{Target: "docker://my-app", Runtime: "docker", Name: "my-app"}); err != nil {
		t.Fatalf("Append should recover from corruption, got: %v", err)
	}

	entries, err := Load()
	if err != nil {
		t.Fatalf("Load after recovery: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "my-app" {
		t.Fatalf("expected the new entry, got %+v", entries)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt file should be kept aside for inspection: %v", err)
	}
}

func TestAppendWritesAtomically(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if err := Append(Entry{Target: "docker://a", Runtime: "docker", Name: "a"}); err != nil {
		t.Fatal(err)
	}
	path, _ := Path()
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "history.json" {
			t.Fatalf("temp file left behind: %s", entry.Name())
		}
	}
}
