package spec

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSkillExamplesValidate is a regression guard for the lwd-toml Claude
// skill (skills/lwd-toml/ at the repo root): every worked-example lwd.toml
// under skills/lwd-toml/references/examples/ must parse and validate
// against this package's real Parse/Validate. If the schema or its
// validation rules ever change, a stale skill example fails here instead of
// silently producing an lwd.toml that lwd apply rejects.
func TestSkillExamplesValidate(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed to resolve this test file's path")
	}
	examplesDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "skills", "lwd-toml", "references", "examples")

	entries, err := os.ReadDir(examplesDir)
	if err != nil {
		t.Fatalf("read examples dir %s: %v", examplesDir, err)
	}

	var found int
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".toml" {
			continue
		}
		found++
		name := entry.Name()
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(examplesDir, name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			app, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse(%s): %v", name, err)
			}
			if err := app.Validate(); err != nil {
				t.Fatalf("Validate(%s): %v", name, err)
			}
		})
	}

	if found == 0 {
		t.Fatalf("no *.toml example files found in %s", examplesDir)
	}
}
