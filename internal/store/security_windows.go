//go:build windows

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ourostack/teamscrawl/internal/errs"
	"golang.org/x/sys/windows"
)

type unsafeArchivePathError struct {
	kind string
	path string
}

func (e *unsafeArchivePathError) Error() string {
	return fmt.Sprintf("unsafe existing %s %s", e.kind, e.path)
}

func mapArchiveOpenError(err error) error {
	var unsafePath *unsafeArchivePathError
	if errors.As(err, &unsafePath) {
		return errs.DBError(err)
	}
	return err
}

func prepareArchiveForWrite(path string) error {
	if err := prepareArchiveDir(path); err != nil {
		return err
	}
	return ensurePrivateArchiveFile(path)
}

func prepareArchiveDir(path string) error {
	parent := filepath.Clean(filepath.Dir(path))
	created, err := createPrivateDirChain(parent)
	if err != nil {
		return err
	}
	if created {
		return nil
	}
	if isDefaultArchiveDir(parent) {
		return securePath(parent, true)
	}
	ok, err := hasPrivateACL(parent)
	if err != nil {
		return err
	}
	if !ok {
		return &unsafeArchivePathError{kind: "custom archive parent", path: parent}
	}
	return nil
}

func finalizeArchiveFile(path string) error {
	return securePath(path, false)
}

func secureLockHandle(f *os.File) error {
	return securePath(f.Name(), false)
}

func ensurePrivateArchiveFile(path string) error {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if info.IsDir() {
			return nil
		}
		ok, err := hasPrivateACL(path)
		if err != nil {
			return err
		}
		if !ok {
			return &unsafeArchivePathError{kind: "archive file", path: path}
		}
		return securePath(path, false)
	case errors.Is(err, os.ErrNotExist):
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // caller chose the archive path
		if err != nil {
			return err
		}
		if cerr := f.Close(); cerr != nil {
			return cerr
		}
		return securePath(path, false)
	default:
		return err
	}
}

func createPrivateDirChain(path string) (bool, error) {
	var missing []string
	for cur := filepath.Clean(path); ; cur = filepath.Dir(cur) {
		info, err := os.Stat(cur)
		switch {
		case err == nil:
			if !info.IsDir() {
				return false, fmt.Errorf("create archive dir: %w", &fs.PathError{Op: "mkdir", Path: cur, Err: windows.ERROR_DIRECTORY})
			}
			goto create
		case errors.Is(err, os.ErrNotExist):
			missing = append(missing, cur)
			next := filepath.Dir(cur)
			if next == cur {
				goto create
			}
		default:
			return false, fmt.Errorf("create archive dir: %w", err)
		}
	}

create:
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o700); err != nil { //nolint:gosec // Windows ACLs enforce the privacy guarantee here
			return len(missing) > 0, fmt.Errorf("create archive dir: %w", err)
		}
		if err := securePath(missing[i], true); err != nil {
			return len(missing) > 0, err
		}
	}
	return len(missing) > 0, nil
}

func securePath(path string, isDir bool) error {
	acl, err := privateACL(isDir)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
}

func hasPrivateACL(path string) (bool, error) {
	// Existing custom parents are safe when their effective DACL is private, even if that privacy
	// is inherited from a private ancestor rather than protected directly on the path itself.
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, nil
	}
	if dacl == nil {
		return false, nil
	}
	want, err := privateTrusteeSet()
	if err != nil {
		return false, err
	}
	got := map[string]struct{}{}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return false, err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false, nil
		}
		got[(*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()] = struct{}{}
	}
	if len(got) != len(want) {
		return false, nil
	}
	for sid := range want {
		if _, ok := got[sid]; !ok {
			return false, nil
		}
	}
	return true, nil
}

var privateSIDState struct {
	once   sync.Once
	user   *windows.SID
	system *windows.SID
	err    error
}

func privateTrusteeSet() (map[string]struct{}, error) {
	user, system, err := privateSIDs()
	if err != nil {
		return nil, err
	}
	return map[string]struct{}{user.String(): {}, system.String(): {}}, nil
}

func privateACL(isDir bool) (*windows.ACL, error) {
	user, system, err := privateSIDs()
	if err != nil {
		return nil, err
	}
	inheritance := uint32(0)
	if isDir {
		inheritance = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	var pinner runtime.Pinner
	pinner.Pin(user)
	pinner.Pin(system)
	defer pinner.Unpin()
	return windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(user),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(system),
			},
		},
	}, nil)
}

func privateSIDs() (*windows.SID, *windows.SID, error) {
	privateSIDState.once.Do(func() {
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			privateSIDState.err = err
			return
		}
		privateSIDState.user, privateSIDState.err = windows.StringToSid(user.User.Sid.String())
		if privateSIDState.err != nil {
			return
		}
		privateSIDState.system, privateSIDState.err = windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	})
	return privateSIDState.user, privateSIDState.system, privateSIDState.err
}

func isDefaultArchiveDir(parent string) bool {
	home := os.Getenv("HOME")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return false
		}
	}
	return filepath.Clean(parent) == filepath.Join(home, ".teamscrawl")
}
