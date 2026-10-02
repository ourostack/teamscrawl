//go:build darwin

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
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errs.Usage("cannot find the home directory; pass --db")
	}
	return filepath.Join(home, ".teamscrawl", "teamscrawl.db"), nil
}

func fullDiskAccessDoctorCheck(code string, coded *errs.Coded) check {
	switch code {
	case errs.CodeTeamsNotInstalled:
		return check{Name: "full_disk_access", Detail: "not checked: Teams is not installed", Fix: "Fix teams_installed first."}
	case errs.CodeNoFullDiskAccess:
		return check{Name: "full_disk_access", Detail: coded.Message, Fix: fdaFix()}
	default:
		return check{Name: "full_disk_access", OK: true, Detail: "the Teams container is readable"}
	}
}

func teamsOriginDoctorCheck(sources []teamsdesktop.Source, other []string, derr error, code string, coded *errs.Coded) check {
	switch {
	case code == errs.CodeNoTeamsOrigin:
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

func outputFix(c *errs.Coded) string {
	if c.Code == errs.CodeNoFullDiskAccess {
		return fdaFix()
	}
	return c.Fix
}
