//go:build windows

package teamsdesktop

import (
	"os"
	"path/filepath"
)

// DefaultRoot is the EBWebView directory of the packaged Windows Teams app.
func DefaultRoot() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "~"
		}
		base = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(base, "Packages", "MSTeams_8wekyb3d8bbwe", "LocalCache", "Microsoft", "MSTeams", "EBWebView")
}
