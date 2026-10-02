// Package errs defines the coded errors shared by every layer. Each code and exit status comes
// from the spec's output contract table; the CLI prints them as {"error": {code, message, fix}}.
package errs

import (
	"fmt"
)

// Exit statuses from the output contract.
const (
	ExitRuntime     = 1
	ExitUsage       = 2
	ExitEnvironment = 3
	ExitLocked      = 4
)

// Error codes from the output contract.
const (
	CodeSnapshotInconsistent        = "snapshot_inconsistent"
	CodeUnsupportedBlockCompression = "unsupported_block_compression"
	CodeStoreMissing                = "store_missing"
	CodeDBError                     = "db_error"
	CodeInternal                    = "internal"
	CodeInterrupted                 = "interrupted"
	CodeUsage                       = "usage"
	CodeTeamsNotInstalled           = "teams_not_installed"
	CodeNoFullDiskAccess            = "no_full_disk_access"
	CodeNoTeamsOrigin               = "no_teams_origin"
	CodeDoctorFailed                = "doctor_failed"
	CodeLocked                      = "locked"
	CodeArchiveNewer                = "archive_newer"
	CodePartialSync                 = "partial_sync"
)

// Coded is an error with a stable machine-readable code, a remedy for the caller and the
// process exit status that goes with it.
type Coded struct {
	Code    string
	Message string
	Fix     string
	Exit    int

	cause error
}

func (e *Coded) Error() string {
	if e.cause != nil {
		return e.Code + ": " + e.Message + ": " + e.cause.Error()
	}
	return e.Code + ": " + e.Message
}

// Unwrap returns the underlying cause, if any.
func (e *Coded) Unwrap() error { return e.cause }

func causeText(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// SnapshotInconsistent reports that the Teams cache kept changing while it was copied.
func SnapshotInconsistent(detail string) *Coded {
	return &Coded{Code: CodeSnapshotInconsistent, Exit: ExitRuntime,
		Message: "could not take a consistent copy of the Teams cache: " + detail,
		Fix:     "Run the command again; if Teams is busy syncing, quit Teams or wait a minute first."}
}

// UnsupportedBlockCompression reports a LevelDB block compression this reader cannot decode.
func UnsupportedBlockCompression(detail string) *Coded {
	return &Coded{Code: CodeUnsupportedBlockCompression, Exit: ExitRuntime,
		Message: "the Teams cache uses a block compression teamscrawl cannot read: " + detail,
		Fix:     "Update teamscrawl; if it is already current, report the issue with the output of `teamscrawl doctor`."}
}

// StoreMissing reports that a Teams store teamscrawl needs disappeared from the cache.
func StoreMissing(detail string) *Coded {
	return &Coded{Code: CodeStoreMissing, Exit: ExitRuntime,
		Message: "a required Teams store is missing: " + detail,
		Fix:     "Open Teams, let it finish loading, and run again; if it persists Teams changed its storage layout, so update teamscrawl."}
}

// DBError wraps an archive database failure.
func DBError(err error) *Coded {
	return &Coded{Code: CodeDBError, Exit: ExitRuntime, cause: err,
		Message: "archive database error",
		Fix:     "Check that the archive path is writable and has free space; run `teamscrawl doctor`."}
}

// Internal wraps an unexpected failure.
func Internal(err error) *Coded {
	return &Coded{Code: CodeInternal, Exit: ExitRuntime, cause: err,
		Message: "internal error: " + causeText(err),
		Fix:     "This is a bug in teamscrawl; report it with the command you ran."}
}

// Usage reports a command-line usage mistake.
func Usage(msg string) *Coded {
	return &Coded{Code: CodeUsage, Exit: ExitUsage, Message: msg,
		Fix: "Run the command with --help to see the accepted arguments and flags."}
}

// TeamsNotInstalled reports that the Teams desktop data directory does not exist.
func TeamsNotInstalled(root string) *Coded {
	return &Coded{Code: CodeTeamsNotInstalled, Exit: ExitEnvironment,
		Message: "Teams desktop data not found at " + root,
		Fix:     "Install the new Microsoft Teams app and sign in once."}
}

// NoFullDiskAccess reports that the OS denied access to the Teams data directory.
func NoFullDiskAccess(path string, err error) *Coded {
	msg, fix := noFullDiskAccessMessage(path)
	return &Coded{Code: CodeNoFullDiskAccess, Exit: ExitEnvironment, cause: err,
		Message: msg,
		Fix:     fix}
}

// NoTeamsOrigin reports that no Teams IndexedDB origin exists under the root.
func NoTeamsOrigin(root string) *Coded {
	return &Coded{Code: CodeNoTeamsOrigin, Exit: ExitEnvironment,
		Message: "no Teams IndexedDB origin found under " + root,
		Fix:     "Open Teams and sign in so it creates its cache, then run again."}
}

// DoctorFailed reports that the environment check found a blocking problem.
func DoctorFailed(msg string) *Coded {
	return &Coded{Code: CodeDoctorFailed, Exit: ExitEnvironment, Message: msg,
		Fix: "Resolve the failing checks listed by `teamscrawl doctor`, then run again."}
}

// ArchiveNewer reports an archive written by a newer teamscrawl (a higher derivation version). An
// older build must not write it: it would mix its own derived fields into rows the newer one keeps.
func ArchiveNewer(found, have int) *Coded {
	return &Coded{Code: CodeArchiveNewer, Exit: ExitEnvironment,
		Message: fmt.Sprintf("this archive was written by a newer teamscrawl (derivation version %d; this build writes version %d)", found, have),
		Fix:     "Upgrade teamscrawl (brew upgrade ourostack/tap/teamscrawl), or point --db at a different archive."}
}

// PartialSync reports a sync in which some sources committed and others failed. detail names the
// failed sources and their error codes. The committed sources' rows are in the archive.
func PartialSync(detail string) *Coded {
	return &Coded{Code: CodePartialSync, Exit: ExitRuntime,
		Message: "some Teams sources synced and others failed: " + detail,
		Fix:     "Run `teamscrawl doctor` to see what is wrong with the failing source, fix it and run `teamscrawl sync` again; the sources that synced are already in the archive."}
}

// Interrupted is a command stopped by SIGINT or SIGTERM. Every write is one transaction per
// source, so a stopped run leaves each source either fully applied or not at all.
func Interrupted() *Coded {
	return &Coded{Code: CodeInterrupted, Exit: ExitRuntime,
		Message: "interrupted before finishing; nothing was half-written",
		Fix:     "Run the command again."}
}

// Locked reports that another teamscrawl run holds the archive lock.
func Locked(msg string) *Coded {
	return &Coded{Code: CodeLocked, Exit: ExitLocked, Message: msg,
		Fix: "Wait for the other teamscrawl run to finish, then run again."}
}

var _ error = (*Coded)(nil)
