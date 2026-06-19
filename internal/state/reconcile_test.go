package state

import "testing"

func TestReconcile(t *testing.T) {
	cases := []struct {
		name                 string
		recorded, remote     string
		found, probe         bool
		wantSource, wantKind string
	}{
		{"not probed", "v1.4.0", "v1.4.0", true, false, SourceRecorded, ""},
		{"match", "1.4.0", "1.4.0", true, true, SourceProbed, ""},
		{"both empty", "", "", false, true, SourceProbed, ""},
		{"behind", "1.4.0", "1.3.0", true, true, SourceDrift, DriftBehind},
		{"ahead", "1.4.0", "1.5.0", true, true, SourceDrift, DriftAhead},
		{"missing", "1.4.0", "", false, true, SourceDrift, DriftMissing},
		{"untracked", "", "1.4.0", true, true, SourceDrift, DriftUntracked},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source, drift := Reconcile(c.recorded, c.remote, c.found, c.probe)
			if source != c.wantSource {
				t.Errorf("source = %q, want %q", source, c.wantSource)
			}
			if c.wantKind == "" {
				if drift != nil {
					t.Errorf("expected no drift, got %+v", drift)
				}
				return
			}
			if drift == nil || drift.Kind != c.wantKind {
				t.Fatalf("drift = %+v, want kind %q", drift, c.wantKind)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.4.0", "1.4.0", 0},
		{"v1.4.0", "1.4.0", 0},
		{"1.3.0", "1.4.0", -1},
		{"1.5.0", "1.4.0", 1},
		{"1.4.1", "1.4.0", 1},
		{"1.4", "1.4.0", 0},
		{"2.0.0", "1.9.9", 1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
