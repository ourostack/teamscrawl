//go:build !windows

package teamsdesktop

import "os"

func runningAsPrivilegedUser() bool { return os.Geteuid() == 0 }

func supportsPermissionDeniedSimulation() bool { return true }
