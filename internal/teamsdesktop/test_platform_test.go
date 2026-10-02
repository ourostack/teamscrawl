package teamsdesktop

import "testing"

func skipIfPermissionDeniedSimulationUnsupported(t *testing.T) {
	t.Helper()
	if !supportsPermissionDeniedSimulation() {
		t.Skip("permission-denied chmod test is not supported on this platform")
	}
	if runningAsPrivilegedUser() {
		t.Skip("permission bits do not restrict the current user")
	}
}
