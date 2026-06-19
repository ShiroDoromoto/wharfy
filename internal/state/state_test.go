package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/build"
)

func sampleArtifacts() []build.Artifact {
	return []build.Artifact{
		{OS: "linux", Arch: "amd64", Path: ".wharfy/dist/x/tool", SHA256: "abc"},
	}
}

func TestLoadMissingReturnsEmpty(t *testing.T) {
	s, err := Load(t.TempDir(), "proj")
	if err != nil {
		t.Fatalf("missing state should not error: %v", err)
	}
	if s.Project != "proj" || s.SchemaVersion != schemaVersion {
		t.Errorf("unexpected empty state: %+v", s)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	root := t.TempDir()
	s, _ := Load(root, "proj")
	s.RecordBuild("v1.2.0", "2026-06-19T00:00:00Z", sampleArtifacts())
	if err := Save(root, s); err != nil {
		t.Fatal(err)
	}

	got, err := Load(root, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastTag != "v1.2.0" {
		t.Errorf("LastTag = %q, want v1.2.0", got.LastTag)
	}
	if got.Build == nil || len(got.Build.Artifacts) != 1 || got.Build.Artifacts[0].OS != "linux" {
		t.Errorf("build record not round-tripped: %+v", got.Build)
	}
	if got.Build.At != "2026-06-19T00:00:00Z" {
		t.Errorf("At = %q", got.Build.At)
	}
}

func TestRecordBuildNoTagKeepsLastTagEmpty(t *testing.T) {
	s := &State{}
	s.RecordBuild("", "2026-06-19T00:00:00Z", sampleArtifacts())
	if s.LastTag != "" {
		t.Errorf("snapshot build (no tag) must not set LastTag, got %q", s.LastTag)
	}
	if s.Build == nil {
		t.Error("build record should still be set")
	}
}

func TestSaveIsAtomicAndValid(t *testing.T) {
	root := t.TempDir()
	s, _ := Load(root, "proj")
	s.RecordBuild("v1", "t", sampleArtifacts())
	if err := Save(root, s); err != nil {
		t.Fatal(err)
	}
	// 最終ファイルは valid JSON。
	b, err := os.ReadFile(Path(root))
	if err != nil {
		t.Fatal(err)
	}
	var check State
	if err := json.Unmarshal(b, &check); err != nil {
		t.Fatalf("state.json is not valid JSON: %v", err)
	}
	// temp ファイルが残っていない(rename で消える)。
	entries, _ := os.ReadDir(filepath.Join(root, DirName))
	for _, e := range entries {
		if e.Name() != FileName {
			t.Errorf("leftover file in .wharfy: %s", e.Name())
		}
	}
}
