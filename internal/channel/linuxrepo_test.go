package channel

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAptProbe(t *testing.T) {
	packages := "Package: widget\nVersion: 1.4.0\nArchitecture: amd64\n\nPackage: other\nVersion: 9.9.9\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dists/stable/main/binary-amd64/Packages" {
			_, _ = w.Write([]byte(packages))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rs, err := (&AptProbe{Repo: srv.URL, HTTP: srv.Client()}).Probe(context.Background(), "widget")
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Found || rs.Version != "1.4.0" {
		t.Errorf("apt probe = %+v, want 1.4.0", rs)
	}

	// レイアウトが違って取得不可 → not found(エラーにしない)。
	rs, err = (&AptProbe{Repo: srv.URL + "/missing", HTTP: srv.Client()}).Probe(context.Background(), "widget")
	if err != nil || rs.Found {
		t.Errorf("missing Packages → not found: rs=%+v err=%v", rs, err)
	}
}

func gz(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return b.Bytes()
}

func TestRpmProbe(t *testing.T) {
	repomd := `<?xml version="1.0"?><repomd><data type="primary"><location href="repodata/primary.xml.gz"/></data></repomd>`
	primary := `<?xml version="1.0"?><metadata><package><name>widget</name><version epoch="0" ver="2.0.1" rel="1"/></package></metadata>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repodata/repomd.xml":
			_, _ = w.Write([]byte(repomd))
		case "/repodata/primary.xml.gz":
			_, _ = w.Write(gz(primary))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	rs, err := (&RpmProbe{Repo: srv.URL, HTTP: srv.Client()}).Probe(context.Background(), "widget")
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Found || rs.Version != "2.0.1" {
		t.Errorf("rpm probe = %+v, want 2.0.1", rs)
	}
}
