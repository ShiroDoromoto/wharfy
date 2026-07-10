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

func TestPS1Version(t *testing.T) {
	body := "$Project   = 'x'\n$Version   = '1.4.0'\n"
	if v := PS1Version(body); v != "1.4.0" {
		t.Errorf("PS1Version = %q, want 1.4.0", v)
	}
	// install.sh の書式を install.ps1 として読むと版は空。取り違えを黙って通さないため。
	if v := PS1Version("#!/bin/sh\nVERSION=\"1.4.0\"\n"); v != "" {
		t.Errorf("PS1Version of an install.sh = %q, want empty", v)
	}
}

// PS1 を立てた Probe は install.ps1 の書式で版を読む。
func TestScriptProbeReadsPS1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("$Version   = '2.0.0'\n"))
	}))
	defer srv.Close()

	rs, err := (&Script{InstallURL: srv.URL, PS1: true}).Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Found || rs.Version != "2.0.0" {
		t.Errorf("probing an install.ps1 should read $Version: %+v", rs)
	}
}
