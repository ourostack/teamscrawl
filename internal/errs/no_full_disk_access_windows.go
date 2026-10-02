//go:build windows

package errs

func noFullDiskAccessMessage(path string) (string, string) {
	return "Windows denied access to " + path,
		"Check that your Windows account can read that Teams data directory, or pass --teams-root with a readable Teams cache path."
}
