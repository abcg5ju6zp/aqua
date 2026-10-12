package vacuum

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/aquaproj/aqua/v2/pkg/config"
	"github.com/aquaproj/aqua/v2/pkg/osfile"
	pkgvacuum "github.com/aquaproj/aqua/v2/pkg/vacuum"
	"github.com/aquaproj/aqua/v2/pkg/vacuum/lock"
	"github.com/suzuki-shunsuke/slog-error/slogerr"
)

// Vacuum removes installed package versions that are neither used nor
// referenced. Every version is processed independently: errors are recorded
// with the package path and the run continues, and an interrupted deletion is
// recovered from the quarantine directory and the journal on the next run.
func (c *Controller) Vacuum(ctx context.Context, logger *slog.Logger, param *config.Param) error {
	runLock, err := c.vacuum.LockVacuum()
	if err != nil {
		return fmt.Errorf("acquire the vacuum lock: %w", err)
	}
	defer func() {
		if err := runLock.Release(); err != nil {
			slogerr.WithError(logger, err).Warn("release the vacuum lock")
		}
	}()

	// The protected set and the "now" timestamp are fixed before any mutation,
	// so leases and references appearing during the run are handled by leases
	// while the configuration snapshot stays stable.
	now := time.Now()
	protected := c.protectedPkgPaths(ctx, logger, param)

	c.reconcile(logger, now, protected)

	timestamps, err := c.vacuum.FindAll(logger)
	if err != nil {
		return fmt.Errorf("find timestamp files: %w", err)
	}
	timestampChecker := pkgvacuum.NewTimestampChecker(now, param.VacuumDays)

	pkgPaths := make([]string, 0, len(timestamps))
	for pkgPath := range timestamps {
		pkgPaths = append(pkgPaths, pkgPath)
	}
	sort.Strings(pkgPaths)
	for _, pkgPath := range pkgPaths {
		c.cleanupOne(logger, pkgPath, timestamps[pkgPath], timestampChecker, now, protected)
	}
	return nil
}

// reconcile finishes or rolls back deletions interrupted by a crash and
// cleans orphan quarantine entries and ledger remnants. It never aborts the run.
func (c *Controller) reconcile(logger *slog.Logger, now time.Time, protected map[string]struct{}) {
	journals := c.readJournals(logger)
	pkgPaths := make([]string, 0, len(journals))
	for pkgPath := range journals {
		pkgPaths = append(pkgPaths, pkgPath)
	}
	sort.Strings(pkgPaths)
	for _, pkgPath := range pkgPaths {
		c.reconcileJournal(logger, pkgPath, journals[pkgPath], now, protected)
	}

	entries, err := c.vacuum.ListQuarantine()
	if err != nil {
		slogerr.WithError(logger, err).Warn("list the quarantine directory, so orphan recovery is skipped")
		return
	}
	// Entries are walked top-down; process them bottom-up so shared parent
	// directories are removed last and restored versions are moved first.
	for i := range slices.Backward(entries) {
		entry := entries[i]
		if _, ok := journals[entry.PkgPath]; ok {
			continue
		}
		c.reconcileOrphan(logger, entry, now, protected)
	}
}

func (c *Controller) readJournals(logger *slog.Logger) map[string]*pkgvacuum.Journal {
	journals, err := c.vacuum.ListJournals(logger)
	if err != nil {
		slogerr.WithError(logger, err).Warn("read the journal, so journal recovery is skipped")
		return map[string]*pkgvacuum.Journal{}
	}
	return journals
}

func (c *Controller) reconcileJournal(logger *slog.Logger, pkgPath string, j *pkgvacuum.Journal, now time.Time, protected map[string]struct{}) {
	logger = logger.With("package_path", pkgPath)
	switch c.reconcileState(logger, pkgPath) {
	case statePlannedOnly:
		// The rename never happened (crash after the planned journal write).
		if err := c.vacuum.RemoveJournal(pkgPath); err != nil {
			slogerr.WithError(logger, err).Warn("remove a journal")
		}
		logger.Debug("dropped a planned journal")
	case stateSourceAndQuarantine:
		c.discardStaleCopy(logger, pkgPath, now)
	case stateQuarantineOnly:
		c.reconcileQuarantined(logger, pkgPath, j, now, protected)
	case stateGone:
		// Both are gone: finish the ledger update interrupted after the
		// physical deletion (the old "remove succeeded, timestamp remove
		// failed" path), cleaning timestamp, leases and the journal.
		if err := c.vacuum.Purge(pkgPath); err != nil {
			slogerr.WithError(logger, err).Warn("purge ledger remnants")
			return
		}
		logger.Debug("cleaned ledger remnants of an already removed package")
	case stateUnknown:
		// The state couldn't be determined; keep the journal for the next run.
	}
}

type reconcileState int

const (
	// stateUnknown means the source/quarantine state couldn't be determined.
	stateUnknown reconcileState = iota
	// statePlannedOnly: source exists, quarantine copy doesn't.
	statePlannedOnly
	// stateSourceAndQuarantine: both exist (a concurrent install won).
	stateSourceAndQuarantine
	// stateQuarantineOnly: only the quarantine copy exists.
	stateQuarantineOnly
	// stateGone: neither the source nor the quarantine copy exists.
	stateGone
)

func (c *Controller) reconcileState(logger *slog.Logger, pkgPath string) reconcileState {
	srcExists, err := osfile.Exists(c.vacuum.SrcPath(pkgPath))
	if err != nil {
		slogerr.WithError(logger, err).Warn("check if the package exists, so the journal is kept")
		return stateUnknown
	}
	qExists, err := c.vacuum.QuarantineExists(pkgPath)
	if err != nil {
		slogerr.WithError(logger, err).Warn("check the quarantine directory, so the journal is kept")
		return stateUnknown
	}
	switch {
	case srcExists && !qExists:
		return statePlannedOnly
	case srcExists:
		return stateSourceAndQuarantine
	case qExists:
		return stateQuarantineOnly
	default:
		return stateGone
	}
}

// discardStaleCopy handles the case where a concurrent install recreated the
// source while the quarantined copy was pending: the install wins.
func (c *Controller) discardStaleCopy(logger *slog.Logger, pkgPath string, now time.Time) {
	if err := c.vacuum.RemoveQuarantined(pkgPath); err != nil {
		slogerr.WithError(logger, err).Warn("remove a stale quarantined package, so the journal is kept")
		return
	}
	if err := c.vacuum.Create(pkgPath, now); err != nil {
		slogerr.WithError(logger, err).Warn("recreate the timestamp of a reinstalled package")
	}
	if err := c.vacuum.RemoveJournal(pkgPath); err != nil {
		slogerr.WithError(logger, err).Warn("remove a journal")
	}
	logger.Info("the package was reinstalled, so discarded the quarantined copy")
}

// reconcileQuarantined restores a quarantined version that is referenced
// again, waits while it is being reinstalled, or finishes the deletion.
func (c *Controller) reconcileQuarantined(logger *slog.Logger, pkgPath string, j *pkgvacuum.Journal, now time.Time, protected map[string]struct{}) {
	if _, ok := protected[pkgPath]; ok {
		c.restoreQuarantined(logger, pkgPath, now)
		return
	}
	if c.leaseActive(logger, pkgPath, now) {
		logger.Debug("the quarantined package is being installed, so it is kept")
		return
	}
	c.finishDeletion(logger, pkgPath, j, now)
}

func (c *Controller) restoreQuarantined(logger *slog.Logger, pkgPath string, now time.Time) {
	if err := c.vacuum.RestoreFromQuarantine(pkgPath); err != nil {
		slogerr.WithError(logger, err).Warn("restore a quarantined package, so the journal is kept")
		return
	}
	if err := c.vacuum.Create(pkgPath, now); err != nil {
		slogerr.WithError(logger, err).Warn("record the timestamp of a restored package")
	}
	if err := c.vacuum.RemoveJournal(pkgPath); err != nil {
		slogerr.WithError(logger, err).Warn("remove a journal")
	}
	logger.Info("restored a quarantined package because it is referenced by the configuration")
}

func (c *Controller) reconcileOrphan(logger *slog.Logger, entry pkgvacuum.QuarantineEntry, now time.Time, protected map[string]struct{}) {
	pkgPath := entry.PkgPath
	if _, ok := protected[pkgPath]; ok {
		c.restoreQuarantined(logger.With("package_path", pkgPath), pkgPath, now)
		return
	}
	if c.leaseActive(logger.With("package_path", pkgPath), pkgPath, now) {
		return
	}
	// Files are deleted directly; directories are removed only once empty,
	// because shared parent directories host other quarantined versions.
	path := c.vacuum.QuarantinePath(pkgPath)
	if entry.IsDir {
		infos, err := os.ReadDir(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				slogerr.WithError(logger, err).Warn("read an orphan quarantine directory", "package_path", pkgPath)
			}
			return
		}
		if len(infos) != 0 {
			return
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		slogerr.WithError(logger, err).Warn("remove an orphan quarantine entry", "package_path", pkgPath)
	}
}

// leaseActive reports whether an install or exec lease blocks the version.
// The exclusive flock proves no local holder is alive; remaining unexpired
// records represent remote hosts sharing the metadata directory. Errors are
// treated conservatively: the version is considered active.
func (c *Controller) leaseActive(logger *slog.Logger, pkgPath string, now time.Time) bool {
	h, err := c.vacuum.AcquireExclusiveLease(pkgPath)
	if err != nil {
		if errors.Is(err, lock.ErrLocked) {
			return true
		}
		slogerr.WithError(logger, err).Warn("acquire an exclusive lease, so the package is treated as active")
		return true
	}
	defer c.releaseLock(logger, h)
	active, err := c.vacuum.HasUnexpiredLease(pkgPath, now)
	if err != nil {
		slogerr.WithError(logger, err).Warn("inspect leases, so the package is treated as active")
		return true
	}
	return active
}

func (c *Controller) releaseLock(logger *slog.Logger, h lock.Handle) {
	if err := h.Release(); err != nil {
		slogerr.WithError(logger, err).Warn("release an exclusive lease")
	}
}

func (c *Controller) cleanupOne(logger *slog.Logger, pkgPath string, timestamp time.Time, checker *pkgvacuum.TimestampChecker, now time.Time, protected map[string]struct{}) {
	logger = logger.With("package_path", pkgPath)
	src := c.vacuum.SrcPath(pkgPath)
	if f, err := osfile.Exists(src); err != nil {
		slogerr.WithError(logger, err).Warn("check if the package exists, so it is skipped")
		return
	} else if !f {
		// The package has been removed manually or by an old aqua version whose
		// ledger update failed: purge the remnants once and stop reprocessing.
		if err := c.vacuum.Purge(pkgPath); err != nil {
			slogerr.WithError(logger, err).Warn("purge the ledger of a missing package")
			return
		}
		logger.Debug("cleaned the ledger of a package that had already been removed")
		return
	}

	if !c.isCandidate(logger, pkgPath, src, timestamp, checker, protected) {
		return
	}
	c.isolateAndDelete(logger, pkgPath, checker, now)
}

// isCandidate runs the lock-free checks: expiration, configuration reference
// and direct shim reference.
func (c *Controller) isCandidate(logger *slog.Logger, pkgPath, src string, timestamp time.Time, checker *pkgvacuum.TimestampChecker, protected map[string]struct{}) bool {
	if !checker.Expired(timestamp) {
		return false
	}
	if _, ok := protected[pkgPath]; ok {
		logger.Debug("the package is referenced by the configuration, so it is kept")
		return false
	}
	if referenced, err := c.vacuum.ReferencedByShim(src); err != nil {
		slogerr.WithError(logger, err).Warn("check shim references, so the package is kept")
		return false
	} else if referenced {
		logger.Info("the package is referenced by a link in the bin directory, so it is kept")
		return false
	}
	return true
}

// isolateAndDelete holds the exclusive lease while re-checking expiration,
// leases and the source, moves the directory into quarantine, then deletes the
// unreachable content and commits the ledger.
func (c *Controller) isolateAndDelete(logger *slog.Logger, pkgPath string, checker *pkgvacuum.TimestampChecker, now time.Time) {
	h, err := c.vacuum.AcquireExclusiveLease(pkgPath)
	if err != nil {
		if errors.Is(err, lock.ErrLocked) {
			logger.Info("the package is being installed or executed, so it is kept")
			return
		}
		slogerr.WithError(logger, err).Warn("acquire an exclusive lease, so the package is kept")
		return
	}
	released := false
	release := func() {
		if !released {
			released = true
			c.releaseLock(logger, h)
		}
	}
	defer release()

	if !c.stillRemovableUnderLock(logger, pkgPath, checker, now) {
		return
	}

	// Phase 1: journal the intent, then atomically isolate the directory.
	j := pkgvacuum.NewJournal(pkgPath, now)
	if err := c.vacuum.SaveJournal(j); err != nil {
		slogerr.WithError(logger, err).Warn("write a journal, so the package is kept")
		return
	}
	if err := c.vacuum.MoveToQuarantine(pkgPath); err != nil {
		slogerr.WithError(logger, err).Warn("move the package to the quarantine directory")
		if rmErr := c.vacuum.RemoveJournal(pkgPath); rmErr != nil {
			slogerr.WithError(logger, rmErr).Warn("remove the planned journal")
		}
		return
	}
	j.SetState(pkgvacuum.JournalQuarantined, time.Now())
	if err := c.vacuum.SaveJournal(j); err != nil {
		// The content is already isolated; recovery deletes it on the next run.
		slogerr.WithError(logger, err).Warn("update the journal, so the deletion is finished on the next run")
	}
	release()

	// Phase 2: delete the unreachable content and update the ledger.
	c.finishDeletion(logger, pkgPath, j, time.Now())
}

// stillRemovableUnderLock repeats every state-dependent check while the
// exclusive lease is held, closing the decide-then-mutate windows.
func (c *Controller) stillRemovableUnderLock(logger *slog.Logger, pkgPath string, checker *pkgvacuum.TimestampChecker, now time.Time) bool {
	active, err := c.vacuum.HasUnexpiredLease(pkgPath, now)
	if err != nil {
		slogerr.WithError(logger, err).Warn("inspect leases, so the package is kept")
		return false
	}
	if active {
		logger.Info("an unexpired lease exists, so the package is kept")
		return false
	}
	// A concurrent exec/install may have renewed the timestamp while waiting
	// for the lock; re-read it under the exclusive lease.
	renewed, ok, err := c.vacuum.ReadTimestamp(pkgPath)
	if err != nil {
		slogerr.WithError(logger, err).Warn("re-read the timestamp, so the package is kept")
		return false
	}
	if !ok {
		logger.Info("the timestamp has disappeared, so the package is kept")
		return false
	}
	if !checker.Expired(renewed) {
		logger.Debug("the timestamp was renewed, so the package is kept")
		return false
	}
	if f, err := osfile.Exists(c.vacuum.SrcPath(pkgPath)); err != nil || !f {
		slogerr.WithError(logger, err).Warn("the package disappeared, so the ledger is purged")
		if err := c.vacuum.Purge(pkgPath); err != nil {
			slogerr.WithError(logger, err).Warn("purge the ledger of a missing package")
		}
		return false
	}
	return true
}

// finishDeletion deletes quarantined content and commits the ledger. A
// reappeared source directory means a concurrent install won and aborts the
// deletion. Any uncertainty keeps the journal for the next run.
func (c *Controller) finishDeletion(logger *slog.Logger, pkgPath string, j *pkgvacuum.Journal, now time.Time) {
	h, err := c.vacuum.AcquireExclusiveLease(pkgPath)
	if err != nil {
		if errors.Is(err, lock.ErrLocked) {
			logger.Info("the quarantined package is being reinstalled, so the deletion is postponed")
			return
		}
		slogerr.WithError(logger, err).Warn("acquire an exclusive lease, so the deletion is postponed")
		return
	}
	defer c.releaseLock(logger, h)

	if f, err := osfile.Exists(c.vacuum.SrcPath(pkgPath)); err != nil {
		slogerr.WithError(logger, err).Warn("check if the package was reinstalled, so the deletion is postponed")
		return
	} else if f {
		c.discardStaleCopy(logger, pkgPath, now)
		return
	}

	if err := c.vacuum.RemoveQuarantined(pkgPath); err != nil {
		// The deletion result is uncertain: keep the journal and retry later.
		j.Fail(now, err)
		if saveErr := c.vacuum.SaveJournal(j); saveErr != nil {
			slogerr.WithError(logger, saveErr).Warn("record the failed deletion in the journal")
		}
		slogerr.WithError(logger, err).Warn("remove the quarantined package, so it is retried on the next run")
		return
	}
	if err := c.vacuum.Purge(pkgPath); err != nil {
		slogerr.WithError(logger, err).Warn("update the ledger; the package is already deleted and the remnants are cleaned on the next run")
		return
	}
	logger.Info("removed the package")
}
