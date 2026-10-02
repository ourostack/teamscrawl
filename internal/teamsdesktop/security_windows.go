//go:build windows

package teamsdesktop

import (
	"os"
	"runtime"
	"sync"

	"golang.org/x/sys/windows"
)

func makeSnapshotRoot() (string, error) {
	dir, err := os.MkdirTemp("", snapshotPrefix)
	if err != nil {
		return "", err
	}
	if err := securePath(dir, true); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func makeSnapshotDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil { //nolint:gosec // Windows ACLs enforce the privacy guarantee here
		return err
	}
	return securePath(path, true)
}

func openSnapshotFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path is inside our private snapshot directory
	if err != nil {
		return nil, err
	}
	if err := securePath(path, false); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
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

var privateSIDState struct {
	once   sync.Once
	user   *windows.SID
	system *windows.SID
	err    error
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
