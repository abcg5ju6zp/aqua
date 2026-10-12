package vacuum

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/aquaproj/aqua/v2/pkg/osfile"
	"github.com/suzuki-shunsuke/slog-error/slogerr"
)

// JournalState is the state of a package version in the deletion journal.
type JournalState string

const (
	// JournalPlanned is written before the directory is renamed into the
	// quarantine directory. Crash recovery with no quarantined directory simply
	// drops the journal.
	JournalPlanned JournalState = "planned"
	// JournalQuarantined means the directory has been moved into quarantine but
	// not deleted yet, or the last deletion attempt failed and must be retried.
	JournalQuarantined JournalState = "quarantined"
	// JournalSuperseded means a concurrent install recreated the package while
	// the quarantined copy was pending; the copy must be discarded.
	JournalSuperseded JournalState = "superseded"
)

const journalFileExt = ".json"

// Journal is the durable state of one package version deletion.
type Journal struct {
	PkgPath   string       `json:"pkg_path"`
	State     JournalState `json:"state"`
	StartedAt time.Time    `json:"started_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	Attempts  int          `json:"attempts,omitempty"`
	LastError string       `json:"last_error,omitempty"`
}

// NewJournal creates a planned journal for the package version.
func NewJournal(pkgPath string, now time.Time) *Journal {
	return &Journal{
		PkgPath:   pkgPath,
		State:     JournalPlanned,
		StartedAt: now,
		UpdatedAt: now,
	}
}

// SetState updates the state and the last update time.
func (j *Journal) SetState(state JournalState, now time.Time) {
	j.State = state
	j.UpdatedAt = now
}

// Fail records a failed deletion attempt and keeps the journal for retry.
func (j *Journal) Fail(now time.Time, cause error) {
	j.Attempts++
	j.LastError = cause.Error()
	j.UpdatedAt = now
}

func journalKey(pkgPath string) string {
	sum := sha256.Sum256([]byte(pkgPath))
	return hex.EncodeToString(sum[:])
}

func (c *Client) journalPath(pkgPath string) string {
	return filepath.Join(c.journalDir(), journalKey(pkgPath)+journalFileExt)
}

// SaveJournal writes the journal atomically (temporary file + rename).
func (c *Client) SaveJournal(j *Journal) error {
	if err := osfile.MkdirAll(c.journalDir()); err != nil {
		return fmt.Errorf("create the journal directory: %w", err)
	}
	body, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal a journal: %w", err)
	}
	if err := osfile.WriteFileAtomic(c.journalPath(j.PkgPath), body, filePermission); err != nil {
		return fmt.Errorf("write a journal: %w", err)
	}
	return nil
}

// RemoveJournal removes the journal entry, tolerating a missing file.
func (c *Client) RemoveJournal(pkgPath string) error {
	if err := os.Remove(c.journalPath(pkgPath)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove a journal: %w", err)
	}
	return nil
}

// ListJournals reads all journal entries keyed by the package path. Entries that
// can't be read or parsed are logged and skipped so that a broken entry never
// blocks the rest of the recovery.
func (c *Client) ListJournals(logger *slog.Logger) (map[string]*Journal, error) {
	entries, err := os.ReadDir(c.journalDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]*Journal{}, nil
		}
		return nil, fmt.Errorf("read the journal directory: %w", err)
	}
	journals := make(map[string]*Journal, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != journalFileExt {
			continue
		}
		path := filepath.Join(c.journalDir(), entry.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			slogerr.WithError(logger, err).Warn("read a journal entry, so skipping it", "journal_file", path)
			continue
		}
		var j Journal
		if err := json.Unmarshal(body, &j); err != nil {
			slogerr.WithError(logger, err).Warn("a journal entry is broken, so skipping it", "journal_file", path)
			continue
		}
		if j.PkgPath == "" {
			slogerr.WithError(logger, errors.New("the pkg_path is empty")).Warn("a journal entry is broken, so skipping it", "journal_file", path)
			continue
		}
		journals[j.PkgPath] = &j
	}
	return journals, nil
}
