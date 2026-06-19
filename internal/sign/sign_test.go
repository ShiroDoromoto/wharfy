package sign

import "testing"

func TestStatusReportsUnsigned(t *testing.T) {
	s := Status([]string{"linux", "darwin", "windows"})

	if _, ok := s["linux"]; ok {
		t.Error("linux should have no signing entry (OS-level signing not applicable)")
	}
	d, ok := s["darwin"]
	if !ok || d.Signed || d.Reason == "" {
		t.Errorf("darwin should be unsigned with a reason: %+v", d)
	}
	w, ok := s["windows"]
	if !ok || w.Signed || w.Reason == "" {
		t.Errorf("windows should be unsigned with a reason: %+v", w)
	}
}

func TestStatusOnlyTargetedOSes(t *testing.T) {
	s := Status([]string{"linux"})
	if len(s) != 0 {
		t.Errorf("linux-only target → no signing entries, got %+v", s)
	}
}
