package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitHub は Releases API の最小モック。tag のリリース有無・アセット置換を検証する。
type fakeGitHub struct {
	releaseExists bool
	prerelease    bool             // 既存リリースが prerelease か(GET が返す)
	assets        map[string]int64 // name → id(既存アセット)
	created       int
	createdPre    bool // create 時に prerelease: true を送ったか
	deleted       []int64
	uploaded      []string
	patched       []map[string]any // PATCH で送った本文(昇格の中身を見る)
}

func (g *fakeGitHub) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	// GET /repos/o/r/releases/tags/{tag}
	mux.HandleFunc("/repos/o/r/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		if !g.releaseExists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeRelease(w, 100, g.assets, g.prerelease)
	})
	// POST /repos/o/r/releases (create)
	mux.HandleFunc("/repos/o/r/releases", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		pre, _ := payload["prerelease"].(bool)
		g.created++
		g.createdPre = pre
		g.releaseExists = true
		g.prerelease = pre
		w.WriteHeader(http.StatusCreated)
		writeRelease(w, 100, nil, pre)
	})
	// PATCH /repos/o/r/releases/{id}
	mux.HandleFunc("/repos/o/r/releases/100", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		g.patched = append(g.patched, payload)
		if pre, ok := payload["prerelease"].(bool); ok {
			g.prerelease = pre
		}
		writeRelease(w, 100, g.assets, g.prerelease)
	})
	// DELETE /repos/o/r/releases/assets/{id}
	mux.HandleFunc("/repos/o/r/releases/assets/", func(w http.ResponseWriter, r *http.Request) {
		var id int64
		fmtSscan(strings.TrimPrefix(r.URL.Path, "/repos/o/r/releases/assets/"), &id)
		g.deleted = append(g.deleted, id)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func writeRelease(w http.ResponseWriter, id int64, assets map[string]int64, prerelease bool) {
	rel := map[string]any{"id": id, "prerelease": prerelease}
	var as []map[string]any
	for name, aid := range assets {
		as = append(as, map[string]any{"id": aid, "name": name})
	}
	rel["assets"] = as
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rel)
}

func fmtSscan(s string, id *int64) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int64(c-'0')
	}
	*id = n
}

// uploadServer は API とアップロードの両方を捌く(uploadsHost も同じ test server に向ける)。
func newStore(t *testing.T, g *fakeGitHub) (*GitHubReleaseStore, *httptest.Server) {
	uploaded := &g.uploaded
	mux := http.NewServeMux()
	api := g.handler(t)
	mux.HandleFunc("/repos/o/r/releases/100/assets", func(w http.ResponseWriter, r *http.Request) {
		*uploaded = append(*uploaded, r.URL.Query().Get("name"))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.Handle("/", api)
	srv := httptest.NewServer(mux)
	s := &GitHubReleaseStore{Owner: "o", Repo: "r", Token: "tok", API: srv.URL, Uploads: srv.URL, HTTP: srv.Client()}
	return s, srv
}

func tmpAsset(t *testing.T, dir, name, content string) ReleaseAsset {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return ReleaseAsset{Name: name, Path: p, ContentType: AssetContentType(name)}
}

// TestReleaseUploadCreatesAndUploads: リリースが無ければ作り、アセットを上げる。
func TestReleaseUploadCreatesAndUploads(t *testing.T) {
	g := &fakeGitHub{releaseExists: false}
	s, srv := newStore(t, g)
	defer srv.Close()
	dir := t.TempDir()
	assets := []ReleaseAsset{
		tmpAsset(t, dir, "app_0.1.0_linux_amd64.tar.gz", "a"),
		tmpAsset(t, dir, "install.sh", "#!/bin/sh"),
	}
	if err := s.Upload(context.Background(), "v0.1.0", "app 0.1.0", assets, ReleaseOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if g.created != 1 {
		t.Errorf("created = %d, want 1", g.created)
	}
	if len(g.uploaded) != 2 {
		t.Errorf("uploaded = %v, want 2 assets", g.uploaded)
	}
	if len(g.deleted) != 0 {
		t.Errorf("deleted = %v, want none (fresh release)", g.deleted)
	}
}

// TestReleaseUploadReplacesExisting: 既存の同名アセットは delete してから再アップロード(冪等)。
func TestReleaseUploadReplacesExisting(t *testing.T) {
	g := &fakeGitHub{releaseExists: true, assets: map[string]int64{"app_0.1.0_linux_amd64.tar.gz": 55}}
	s, srv := newStore(t, g)
	defer srv.Close()
	dir := t.TempDir()
	assets := []ReleaseAsset{tmpAsset(t, dir, "app_0.1.0_linux_amd64.tar.gz", "a")}
	if err := s.Upload(context.Background(), "v0.1.0", "app 0.1.0", assets, ReleaseOptions{Prerelease: true}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if g.prerelease {
		t.Error("uploading to an existing release must not turn it into a prerelease (promotion is a separate, explicit step)")
	}
	if g.created != 0 {
		t.Errorf("created = %d, want 0 (release existed)", g.created)
	}
	if len(g.deleted) != 1 || g.deleted[0] != 55 {
		t.Errorf("deleted = %v, want [55]", g.deleted)
	}
	if len(g.uploaded) != 1 {
		t.Errorf("uploaded = %v, want 1", g.uploaded)
	}
}

// TestReleaseUploadNeedsToken: token 無しは即失敗。
func TestReleaseUploadNeedsToken(t *testing.T) {
	s := &GitHubReleaseStore{Owner: "o", Repo: "r"}
	if err := s.Upload(context.Background(), "v0.1.0", "", nil, ReleaseOptions{}); err == nil {
		t.Fatal("expected error without token")
	}
}

func TestInMemoryReleaseStore(t *testing.T) {
	s := NewInMemoryReleaseStore()
	_ = s.Upload(context.Background(), "v1", "", []ReleaseAsset{{Name: "a"}, {Name: "b"}}, ReleaseOptions{Prerelease: true})
	_ = s.Upload(context.Background(), "v1", "", []ReleaseAsset{{Name: "a"}}, ReleaseOptions{})
	if s.Uploads != 3 || s.Replaced != 1 {
		t.Errorf("uploads=%d replaced=%d, want 3/1", s.Uploads, s.Replaced)
	}
	st, _ := s.Get(context.Background(), "v1")
	if !st.Exists || !st.Prerelease {
		t.Errorf("Get(v1) = %+v, want exists+prerelease (the second upload must not change it)", st)
	}
}

// TestReleaseUploadCreatesPrerelease: 無ければ prerelease として作る(latest にならない)。
func TestReleaseUploadCreatesPrerelease(t *testing.T) {
	g := &fakeGitHub{releaseExists: false}
	s, srv := newStore(t, g)
	defer srv.Close()
	dir := t.TempDir()
	assets := []ReleaseAsset{tmpAsset(t, dir, "app_0.1.0_linux_amd64.tar.gz", "a")}
	if err := s.Upload(context.Background(), "v0.1.0", "app 0.1.0", assets, ReleaseOptions{Prerelease: true}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !g.createdPre {
		t.Error("created the release without prerelease: true — it would become github's latest at once")
	}
}

// TestReleaseGet: tag のリリースの現状(無い/在る/prerelease)を、上げる前に読めること。
func TestReleaseGet(t *testing.T) {
	g := &fakeGitHub{releaseExists: false}
	s, srv := newStore(t, g)
	defer srv.Close()
	st, err := s.Get(context.Background(), "v0.1.0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.Exists {
		t.Errorf("Get on a missing release = %+v, want not exists", st)
	}
	g.releaseExists, g.prerelease = true, true
	st, err = s.Get(context.Background(), "v0.1.0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !st.Exists || !st.Prerelease {
		t.Errorf("Get = %+v, want exists+prerelease", st)
	}
}

// TestReleasePromote: prerelease を latest に切り替える。資産は触らず、フラグだけを送る。
func TestReleasePromote(t *testing.T) {
	g := &fakeGitHub{releaseExists: true, prerelease: true}
	s, srv := newStore(t, g)
	defer srv.Close()

	changed, err := s.Promote(context.Background(), "v0.1.0")
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true (it was a prerelease)")
	}
	if len(g.patched) != 1 {
		t.Fatalf("patched = %v, want exactly one edit", g.patched)
	}
	if g.patched[0]["prerelease"] != false || g.patched[0]["make_latest"] != "true" {
		t.Errorf("patch body = %v, want prerelease:false + make_latest:true", g.patched[0])
	}
	if len(g.uploaded) != 0 || len(g.deleted) != 0 {
		t.Error("promotion must not touch a single asset (the verified bytes are the shipped bytes)")
	}
}

// TestReleasePromoteIdempotent: 既に latest なら API を叩かないで緑。
func TestReleasePromoteIdempotent(t *testing.T) {
	g := &fakeGitHub{releaseExists: true, prerelease: false}
	s, srv := newStore(t, g)
	defer srv.Close()

	changed, err := s.Promote(context.Background(), "v0.1.0")
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if changed || len(g.patched) != 0 {
		t.Errorf("already latest: changed=%v patched=%v, want no-op", changed, g.patched)
	}
}

// TestReleasePromoteNoRelease: 上がっていない版は昇格できない(ErrNoRelease)。
func TestReleasePromoteNoRelease(t *testing.T) {
	g := &fakeGitHub{releaseExists: false}
	s, srv := newStore(t, g)
	defer srv.Close()

	if _, err := s.Promote(context.Background(), "v0.1.0"); !errors.Is(err, ErrNoRelease) {
		t.Errorf("err = %v, want ErrNoRelease", err)
	}
}
