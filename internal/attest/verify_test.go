package attest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/golang/snappy"
)

// fakeFetcher は digest → 預けてある bundle。err はネットワーク/API の失敗。
type fakeFetcher struct {
	bundles map[string][][]byte
	err     error
	asked   []string
}

func (f *fakeFetcher) Bundles(_ context.Context, sha256 string) ([][]byte, error) {
	f.asked = append(f.asked, sha256)
	if f.err != nil {
		return nil, f.err
	}
	return f.bundles[sha256], nil
}

// fakeBundleVerifier は bad に入っている bundle だけ検算に失敗する。
type fakeBundleVerifier struct{ bad map[string]bool }

func (v fakeBundleVerifier) VerifyBundle(_ context.Context, b []byte, _ string, _ Identity) error {
	if v.bad[string(b)] {
		return errors.New("no rekor entry")
	}
	return nil
}

func subj(name, digest string) Subject { return Subject{Name: name, SHA256: digest} }

// 全 subject に検算できる証明が在る → Verified。
func TestVerifyAllSubjectsAttested(t *testing.T) {
	f := &fakeFetcher{bundles: map[string][][]byte{
		"aa": {[]byte("bundle-a")},
		"bb": {[]byte("bundle-b")},
	}}
	cov, err := Verify(context.Background(), []Subject{subj("a", "aa"), subj("b", "bb")},
		ActionsIdentity("acme/demo"), f, fakeBundleVerifier{})
	if err != nil {
		t.Fatalf("looking up provenance should not fail: %v", err)
	}
	if !cov.Verified() || len(cov.Missing()) != 0 || len(cov.Broken()) != 0 {
		t.Fatalf("every subject carries verifiable provenance: %+v", cov)
	}
}

// subject ごとに引く。1 つの証言に全部載っていても、載せ損ねた 1 つは「その digest で何も引けない」
// 形でしか現れない —— まとめて 1 回引くとその取りこぼしが見えない。
func TestVerifyLooksEverySubjectUp(t *testing.T) {
	f := &fakeFetcher{bundles: map[string][][]byte{"aa": {[]byte("b")}, "bb": {[]byte("b")}}}
	_, _ = Verify(context.Background(), []Subject{subj("a", "aa"), subj("b", "bb")},
		ActionsIdentity("acme/demo"), f, fakeBundleVerifier{})
	if len(f.asked) != 2 || f.asked[0] != "aa" || f.asked[1] != "bb" {
		t.Fatalf("each subject's digest must be looked up on its own: %+v", f.asked)
	}
}

// 証明が預けられていない subject は Missing(検算の失敗ではない)。
func TestVerifyReportsSubjectsWithoutProvenance(t *testing.T) {
	f := &fakeFetcher{bundles: map[string][][]byte{"aa": {[]byte("bundle-a")}}}
	cov, err := Verify(context.Background(), []Subject{subj("a", "aa"), subj("b", "bb")},
		ActionsIdentity("acme/demo"), f, fakeBundleVerifier{})
	if err != nil {
		t.Fatalf("a subject without provenance is an observation, not an error: %v", err)
	}
	if cov.Verified() {
		t.Error("a release that is only partly attested is not verified")
	}
	if len(cov.Missing()) != 1 || cov.Missing()[0].Subject.Name != "b" {
		t.Fatalf("the subject without provenance should be named: %+v", cov.Missing())
	}
	if len(cov.Broken()) != 0 {
		t.Errorf("nothing was broken — it was simply never attested: %+v", cov.Broken())
	}
}

// 証明は在るのに検算できない → Broken(Missing と混ぜない。読み手にとって別の事件)。
func TestVerifySeparatesBrokenFromMissing(t *testing.T) {
	f := &fakeFetcher{bundles: map[string][][]byte{"aa": {[]byte("bad")}}}
	cov, err := Verify(context.Background(), []Subject{subj("a", "aa")},
		ActionsIdentity("acme/demo"), f, fakeBundleVerifier{bad: map[string]bool{"bad": true}})
	if err != nil {
		t.Fatalf("a bundle that does not verify is a finding, not a transport failure: %v", err)
	}
	if len(cov.Broken()) != 1 || len(cov.Missing()) != 0 {
		t.Fatalf("provenance that exists but does not verify is broken, not missing: %+v", cov)
	}
	if cov.Broken()[0].Err == nil {
		t.Error("the reason it did not verify must survive to the report")
	}
}

// 同じ digest に証明が複数預けられることはある(再リリース・別の証言)。1 つ通れば来歴は在る。
func TestVerifyAcceptsAnyBundleThatVerifies(t *testing.T) {
	f := &fakeFetcher{bundles: map[string][][]byte{"aa": {[]byte("bad"), []byte("good")}}}
	cov, err := Verify(context.Background(), []Subject{subj("a", "aa")},
		ActionsIdentity("acme/demo"), f, fakeBundleVerifier{bad: map[string]bool{"bad": true}})
	if err != nil || !cov.Verified() {
		t.Fatalf("one bundle that verifies is enough: %+v (%v)", cov, err)
	}
}

// 取り口が落ちた(ネットワーク・API)のを「証明が無い」と読み替えない —— 読み替えると、
// 落ちている間ずっと「まだ付けていない」と報告し続ける。
func TestVerifyDoesNotMistakeATransportFailureForMissingProvenance(t *testing.T) {
	f := &fakeFetcher{err: errors.New("503 service unavailable")}
	_, err := Verify(context.Background(), []Subject{subj("a", "aa")},
		ActionsIdentity("acme/demo"), f, fakeBundleVerifier{})
	if err == nil {
		t.Fatal("a failure to reach the attestations api must not read as 'no provenance'")
	}
}

// 縛るのは「そのリポジトリの workflow が署名したこと」。名前の似た別リポジトリの workflow を通さない。
func TestActionsIdentityBindsTheSigningRepository(t *testing.T) {
	id := ActionsIdentity("acme/demo")
	if id.Issuer != ActionsIssuer {
		t.Errorf("the issuer must be the github actions oidc issuer: %q", id.Issuer)
	}
	re := regexp.MustCompile(id.SANRegexp)
	if !re.MatchString("https://github.com/acme/demo/.github/workflows/release.yml@refs/tags/v1.2.0") {
		t.Error("this repository's own workflow must match")
	}
	for _, san := range []string{
		"https://github.com/evil/demo/.github/workflows/release.yml@refs/tags/v1.2.0",
		"https://github.com/acme/demo-evil/.github/workflows/release.yml@refs/tags/v1.2.0",
	} {
		if re.MatchString(san) {
			t.Errorf("a workflow from another repository must not pass as ours: %s", san)
		}
	}
}

// GitHub は bundle を本文に載せず、署名付きの blob URL(snappy で縮めてある)だけを返すことがある。
// そこまで追えないと、検算は「証明が無い」と言い出す。
func TestBundlesFollowsTheBlobURLAndDecompresses(t *testing.T) {
	const bundleJSON = `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`
	var srv *httptest.Server
	var blobAuth string
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/demo/attestations/sha256:aa":
			if got := r.URL.Query().Get("predicate_type"); got != predicateType {
				t.Errorf("only build provenance should be asked for, not every attestation: %q", got)
			}
			_, _ = w.Write([]byte(`{"attestations":[{"bundle":null,"bundle_url":"` + srv.URL + `/blob"}]}`))
		case "/blob":
			blobAuth = r.Header.Get("Authorization")
			_, _ = w.Write(snappy.Encode(nil, []byte(bundleJSON)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := &GitHubStore{Owner: "acme", Repo: "demo", Token: "secret-token", API: srv.URL}
	bundles, err := s.Bundles(context.Background(), "aa")
	if err != nil {
		t.Fatalf("a bundle stored as a blob must still be readable: %v", err)
	}
	if len(bundles) != 1 || string(bundles[0]) != bundleJSON {
		t.Fatalf("the bundle should come back decompressed: %q", bundles)
	}
	// blob は GitHub ではない別のホストで、URL 自体が署名付き。リポジトリのトークンを渡さない。
	if blobAuth != "" {
		t.Errorf("the repository token must not be sent to the blob host: %q", blobAuth)
	}
}

// 証明が 1 つも無い digest は 404 で返る。エラーにすると「まだ付けていない」が報告できない。
func TestBundlesTreatsNoAttestationAsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	s := &GitHubStore{Owner: "acme", Repo: "demo", API: srv.URL}
	bundles, err := s.Bundles(context.Background(), "aa")
	if err != nil || len(bundles) != 0 {
		t.Fatalf("a digest with no attestation is empty, not an error: %v %q", err, bundles)
	}
}

// 本文に bundle が載っているならそれを使う(blob を追いに行かない)。
func TestBundlesUsesTheInlineBundle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"attestations":[{"bundle":{"mediaType":"x"},"bundle_url":"http://unreachable.invalid/blob"}]}`))
	}))
	defer srv.Close()

	s := &GitHubStore{Owner: "acme", Repo: "demo", API: srv.URL}
	bundles, err := s.Bundles(context.Background(), "aa")
	if err != nil {
		t.Fatalf("an inline bundle needs no blob fetch: %v", err)
	}
	if len(bundles) != 1 || !strings.Contains(string(bundles[0]), `"mediaType":"x"`) {
		t.Fatalf("the inline bundle should be returned verbatim: %q", bundles)
	}
}
