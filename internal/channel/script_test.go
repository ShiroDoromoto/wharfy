package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestScriptVersion(t *testing.T) {
	body := "#!/bin/sh\nPROJECT=\"x\"\nVERSION=\"1.4.0\"\n"
	if v := ScriptVersion(body); v != "1.4.0" {
		t.Errorf("ScriptVersion = %q, want 1.4.0", v)
	}
	if v := ScriptVersion("no version"); v != "" {
		t.Errorf("ScriptVersion of junk = %q", v)
	}
}

func TestScriptProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/install.sh" {
			_, _ = w.Write([]byte("#!/bin/sh\nVERSION=\"2.0.0\"\n"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	s := &Script{InstallURL: srv.URL + "/install.sh", HTTP: srv.Client()}
	rs, err := s.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Found || rs.Version != "2.0.0" {
		t.Errorf("probe = %+v, want found 2.0.0", rs)
	}

	s.InstallURL = srv.URL + "/missing.sh"
	rs, err = s.Probe(context.Background())
	if err != nil || rs.Found {
		t.Errorf("404 should be not-found without error: rs=%+v err=%v", rs, err)
	}
}
