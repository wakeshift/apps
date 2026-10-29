package appkit_test

import (
	"os"
	"path/filepath"
)

// sharedGolden is the path of a golden file the three SDKs share: beside
// the TypeScript SDK's tests in Wakeshift's repository, or in
// testdata/golden in the published Go SDK, where opensource/export.sh
// copies them (the internal tests' sharedGoldenDir).
func sharedGolden(name string) string {
	here := filepath.Join("..", "..", "sdk", "typescript", "test")
	if _, err := os.Stat(here); err == nil {
		return filepath.Join(here, name)
	}
	return filepath.Join("testdata", "golden", name)
}
