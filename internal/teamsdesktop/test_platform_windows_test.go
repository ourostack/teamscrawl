//go:build windows

package teamsdesktop

func runningAsPrivilegedUser() bool { return false }

func supportsPermissionDeniedSimulation() bool { return false }
