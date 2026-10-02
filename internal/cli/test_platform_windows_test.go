//go:build windows

package cli

func runningAsPrivilegedUser() bool { return false }

func supportsPermissionDeniedSimulation() bool { return false }
