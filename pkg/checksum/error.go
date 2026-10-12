package checksum

import "errors"

var (
	// errInvalidChecksum indicates that a checksum value is malformed or incorrect.
	errInvalidChecksum = errors.New("checksum is invalid")

	// errUnknownChecksumFileFormat indicates that the checksum file format is not recognized.
	errUnknownChecksumFileFormat = errors.New("checksum file format is unknown")

	// ErrNoChecksumExtracted indicates that no checksum could be extracted from the source.
	ErrNoChecksumExtracted = errors.New("no checksum is extracted")

	// ErrNoChecksumIsFound indicates that no checksum was found for the requested resource.
	ErrNoChecksumIsFound = errors.New("no checksum is found")

	// ErrChecksumConflict classifies cases where this process confirmed a
	// checksum for an ID that another process has persisted with a different
	// value or algorithm. The file is left unchanged until the conflict is
	// resolved.
	ErrChecksumConflict = errors.New("checksum conflict")

	// ErrPruneDeferred indicates that confirmed additions were committed but
	// pruning was skipped because the configuration file changed while the
	// command was running. Running update-checksum with pruning again
	// completes the deletions.
	ErrPruneDeferred = errors.New("the configuration file changed while aqua was running, so unused checksums weren't pruned; run `aqua update-checksum --prune` again to prune them")
)
