package configdir

import (
	"os"
	"path/filepath"
	"runtime"
)

// Path returns the platform-appropriate configuration directory for kojo.
//   - Windows: %APPDATA%\kojo
//   - Others:  ~/.config/kojo
func Path() string {
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "kojo")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "kojo")
	}
	return filepath.Join(home, ".config", "kojo")
}
