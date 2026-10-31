package testreport

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxXMLFiles = 500
	maxXMLBytes = 32 << 20
)

// JUnitFiles reads the JUnit reports under root written at or after since:
// junit*.xml and TEST-*.xml anywhere, and any .xml under surefire-reports/,
// failsafe-reports/ or test-results/ (maven, gradle). Older files are another
// run's (a dev tree, or a fixture the repo tracks) and never count.
func JUnitFiles(root string, since time.Time) [][]byte {
	since = since.Add(-2 * time.Second) // coarse mtimes; a container's clock
	var out [][]byte
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".venv", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		if len(out) == maxXMLFiles || !isReport(p) {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxXMLBytes || fi.ModTime().Before(since) {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil {
			out = append(out, b)
		}
		return nil
	})
	return out
}

func isReport(p string) bool {
	base := filepath.Base(p)
	if !strings.HasSuffix(base, ".xml") {
		return false
	}
	if strings.HasPrefix(strings.ToLower(base), "junit") || strings.HasPrefix(base, "TEST-") {
		return true
	}
	for _, dir := range strings.Split(filepath.ToSlash(filepath.Dir(p)), "/") {
		if dir == "surefire-reports" || dir == "failsafe-reports" || dir == "test-results" {
			return true
		}
	}
	return false
}
