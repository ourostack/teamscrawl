//go:build !darwin && !windows

package teamsdesktop

import (
	"os"
	"path/filepath"
)

// DefaultRoot is the EBWebView directory of the new Teams app in its macOS container.
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "~"
	}
	return filepath.Join(home, "Library", "Containers", "com.microsoft.teams2", "Data", "Library",
		"Application Support", "Microsoft", "MSTeams", "EBWebView")
}
