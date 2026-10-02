// Package teamsdesktop reads the Microsoft Teams desktop app's local cache: it finds the
// IndexedDB origin, takes a safe copy, fingerprints changes, and iterates the allowlisted
// databases. Nothing here writes to Teams' storage.
package teamsdesktop

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ourostack/teamscrawl/internal/errs"
)

var (
	statDir        = os.Stat
	readDirEntries = os.ReadDir
)

// Source is one Teams IndexedDB origin inside one WebView2 profile.
type Source struct {
	Profile    string // profile directory name under the EBWebView root
	Origin     string // origin directory name without the .indexeddb suffix
	LevelDBDir string
	BlobDir    string
}

// Key identifies the source in the archive: "<profile>|<origin>".
func (s Source) Key() string { return s.Profile + "|" + s.Origin }

var teamsOrigin = regexp.MustCompile(`^https_teams\.(microsoft\.com|cloud\.microsoft)_\d+$`)

const (
	levelDBSuffix = ".indexeddb.leveldb"
	blobSuffix    = ".indexeddb.blob"
)

// Discover lists the Teams origins under root (an EBWebView directory). otherOrigins names the
// non-Teams IndexedDB origins it saw and ignored. Errors are *errs.Coded: teams_not_installed
// when root is missing, no_full_disk_access when the OS denies access, no_teams_origin when no
// profile holds a Teams origin.
func Discover(root string) (sources []Source, otherOrigins []string, err error) {
	profiles, err := readDir(root, root)
	if err != nil {
		return nil, nil, err
	}
	otherSet := map[string]bool{}
	for _, p := range profiles {
		if !p.IsDir() {
			continue
		}
		idb := filepath.Join(root, p.Name(), "IndexedDB")
		ents, err := readDir(root, idb)
		if err != nil {
			var coded *errs.Coded
			if errors.As(err, &coded) && coded.Code == errs.CodeTeamsNotInstalled {
				continue // a profile without IndexedDB
			}
			return nil, nil, err
		}
		for _, e := range ents {
			origin, ok := strings.CutSuffix(e.Name(), levelDBSuffix)
			if !ok || !e.IsDir() {
				continue
			}
			if !teamsOrigin.MatchString(origin) {
				otherSet[origin] = true
				continue
			}
			sources = append(sources, Source{
				Profile:    p.Name(),
				Origin:     origin,
				LevelDBDir: filepath.Join(idb, e.Name()),
				BlobDir:    filepath.Join(idb, origin+blobSuffix),
			})
		}
	}
	for o := range otherSet {
		otherOrigins = append(otherOrigins, o)
	}
	sort.Strings(otherOrigins)
	sort.Slice(sources, func(i, j int) bool { return sources[i].Key() < sources[j].Key() })
	if len(sources) == 0 {
		return nil, otherOrigins, errs.NoTeamsOrigin(root)
	}
	return sources, otherOrigins, nil
}

// readDir lists dir, mapping failures to coded errors. A missing root is teams_not_installed;
// any other missing directory is reported the same way so callers can skip it.
func readDir(root, dir string) ([]fs.DirEntry, error) {
	info, err := statDir(dir)
	if err == nil && !info.IsDir() {
		// Windows maps os.ReadDir(file) to fs.ErrNotExist, which would incorrectly turn a file root
		// into teams_not_installed without this explicit directory check.
		return nil, errs.Internal(&fs.PathError{Op: "readdir", Path: dir, Err: fs.ErrInvalid})
	}
	ents, err := readDirEntries(dir)
	if err == nil {
		return ents, nil
	}
	if errors.Is(err, fs.ErrPermission) {
		return nil, errs.NoFullDiskAccess(dir, err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errs.TeamsNotInstalled(root)
	}
	return nil, errs.Internal(err)
}
