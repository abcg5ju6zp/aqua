package vacuum_test

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aquaproj/aqua/v2/pkg/config"
	"github.com/aquaproj/aqua/v2/pkg/vacuum"
)

func TestJournal_saveListRemove(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	client := vacuum.New(&config.Param{RootDir: rootDir})
	logger := slog.New(slog.DiscardHandler)
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

	j := vacuum.NewJournal(leasePkgPath, now)
	if err := client.SaveJournal(j); err != nil {
		t.Fatal(err)
	}

	// The journal file name is the hash of the package path.
	sum := sha256.Sum256([]byte(leasePkgPath))
	expPath := filepath.Join(rootDir, "metadata", "vacuum", "journal", hex.EncodeToString(sum[:])+".json")
	if _, err := os.Stat(expPath); err != nil {
		t.Fatalf("the journal must be written at the hashed path: %v", err)
	}

	j.SetState(vacuum.JournalQuarantined, now.Add(time.Minute))
	if err := client.SaveJournal(j); err != nil {
		t.Fatal(err)
	}
	journals, err := client.ListJournals(logger)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := journals[leasePkgPath]
	if !ok {
		t.Fatal("the journal must be listed")
	}
	if got.State != vacuum.JournalQuarantined {
		t.Fatalf("state = %s, want %s", got.State, vacuum.JournalQuarantined)
	}

	if err := client.RemoveJournal(leasePkgPath); err != nil {
		t.Fatal(err)
	}
	journals, err = client.ListJournals(logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := journals[leasePkgPath]; ok {
		t.Fatal("the journal must be removed")
	}
}

func TestListJournals_brokenEntryIsSkipped(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	client := vacuum.New(&config.Param{RootDir: rootDir})
	logger := slog.New(slog.DiscardHandler)

	dir := filepath.Join(rootDir, "metadata", "vacuum", "journal")
	if err := os.MkdirAll(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	journals, err := client.ListJournals(logger)
	if err != nil {
		t.Fatalf("a broken entry must not fail the listing: %v", err)
	}
	if len(journals) != 0 {
		t.Fatalf("broken entries must be skipped, got %d", len(journals))
	}
}
