//go:build !windows

package cli

import "os"

func runningAsPrivilegedUser() bool { return os.Geteuid() == 0 }

func supportsPermissionDeniedSimulation() bool { return true }
