package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVersionUsesArtifactManifestWhenEnvironmentIsUnset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact-manifest.json")
	if err := os.WriteFile(path, []byte(`{"source_commit":"deadbeef1234"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_VERSION", "")
	t.Setenv("LY_ROUTE_ARTIFACT_MANIFEST", path)
	if got := Version(); got != "deadbeef1234" {
		t.Fatalf("Version()=%q, want deadbeef1234", got)
	}
}

func TestVersionPrefersExplicitRelease(t *testing.T) {
	t.Setenv("LY_ROUTE_VERSION", "v7.0.0")
	if got := Version(); got != "v7.0.0" {
		t.Fatalf("Version()=%q, want v7.0.0", got)
	}
}
