//go:build windows

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ourostack/teamscrawl/internal/errs"
	"github.com/ourostack/teamscrawl/internal/teamsdesktop"
)

func defaultArchivePath() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errs.Usage("cannot find %LOCALAPPDATA% or the home directory; pass --db")
		}
		base = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(base, "teamscrawl", "teamscrawl.db"), nil
}

func fullDiskAccessDoctorCheck(string, *errs.Coded) check {
	return check{Name: "full_disk_access", OK: true, Detail: "not applicable on Windows"}
}

func teamsOriginDoctorCheck(sources []teamsdesktop.Source, other []string, derr error, code string, coded *errs.Coded) check {
	switch {
	case code == errs.CodeNoTeamsOrigin:
		return check{Name: "teams_origin", Detail: coded.Message, Fix: coded.Fix}
	case code == errs.CodeNoFullDiskAccess:
		return check{Name: "teams_origin", Detail: coded.Message, Fix: coded.Fix}
	case derr != nil:
		return check{Name: "teams_origin", Detail: "not checked: the Teams data is not readable", Fix: "Fix the checks above first."}
	default:
		d := fmt.Sprintf("%d Teams origin(s) found", len(sources))
		if len(other) > 0 {
			d += "; ignoring non-Teams origins: " + strings.Join(other, ", ")
		}
		return check{Name: "teams_origin", OK: true, Detail: d}
	}
}

func outputFix(c *errs.Coded) string { return c.Fix }
