package attest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeTokens / fakeSigner / fakeStore — Sigstore にも GitHub にも触らずに段の筋を確かめる。
type fakeTokens struct {
	token    string
	audience string
	err      error
}

func (f *fakeTokens) IDToken(_ context.Context, audience string) (string, error) {
	f.audience = audience
	return f.token, f.err
}

type fakeSigner struct {
	statement []byte
	idToken   string
	err       error
}

func (f *fakeSigner) SignDSSE(_ context.Context, statement []byte, idToken string) ([]byte, error) {
	f.statement, f.idToken = statement, idToken
	return []byte(`{"mediaType":"bundle"}`), f.err
}

type fakeStore struct {
	bundle []byte
	err    error
}

func (f *fakeStore) Put(_ context.Context, bundleJSON []byte) (int64, error) {
	f.bundle = bundleJSON
	return 42, f.err
}

func ciOptions() Options {
	return Options{
		Repo:  "acme/widget",
		Token: "t",
		OIDC:  OIDCEnv{RequestURL: "https://oidc.example/token", RequestToken: "rt"},
		Env: Env{
			Repository: "acme/widget", ServerURL: "https://github.com", SHA: "abc123",
			Ref: "refs/tags/v1.2.3", WorkflowRef: "acme/widget/.github/workflows/release.yml@refs/tags/v1.2.3",
			EventName: "push", RunID: "7", RunAttempt: "1",
		},
	}
}

// TestEnabledNeedsOIDCTokenAndRepo: 3 つ揃って初めて証明できる(どれが欠けても no-op)。
func TestEnabledNeedsOIDCTokenAndRepo(t *testing.T) {
	full := ciOptions()
	if !full.Enabled() {
		t.Fatal("full options should be enabled")
	}
	noOIDC := full
	noOIDC.OIDC = OIDCEnv{}
	noToken := full
	noToken.Token = ""
	noRepo := full
	noRepo.Repo = ""
	for name, o := range map[string]Options{"no oidc": noOIDC, "no token": noToken, "no repo": noRepo} {
		if o.Enabled() {
			t.Errorf("%s: should not be enabled", name)
		}
	}
}

// TestStatusExplainsWhyNotAvailable: 証明できないときは必ず理由を持つ(黙って no-op しない)。
// 証明が及ばない範囲も常に出す——「全部に来歴が付く」と読まれたら status が嘘をついたことになる。
func TestStatusExplainsWhyNotAvailable(t *testing.T) {
	st := Status(Options{})
	if st.Available {
		t.Fatal("no environment: must not claim provenance is available")
	}
	if st.Reason == "" {
		t.Fatal("unavailable without a reason")
	}
	if len(st.Uncovered) == 0 {
		t.Fatal("status must always say what provenance does NOT cover")
	}

	ok := Status(ciOptions())
	if !ok.Available || ok.Reason != "" {
		t.Fatalf("full CI options: want available with no reason, got %+v", ok)
	}
}

// TestAttestSignsTheStatementAndStoresTheBundle: 段の筋(OIDC → 証言 → 署名 → 預ける)。
func TestAttestSignsTheStatementAndStoresTheBundle(t *testing.T) {
	tokens := &fakeTokens{token: "id-token"}
	signer := &fakeSigner{}
	store := &fakeStore{}
	subjects := []Subject{{Name: "widget_1.2.3_darwin_arm64.tar.gz", SHA256: "deadbeef"}}

	res, err := Attest(context.Background(), ciOptions(), subjects, tokens, signer, store)
	if err != nil {
		t.Fatalf("attest: %v", err)
	}
	if res.ID != 42 || len(res.Subjects) != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if tokens.audience != "sigstore" {
		t.Errorf("fulcio only accepts the sigstore audience, got %q", tokens.audience)
	}
	if signer.idToken != "id-token" {
		t.Errorf("the OIDC token must reach the signer, got %q", signer.idToken)
	}
	if !strings.Contains(string(signer.statement), "deadbeef") {
		t.Errorf("the statement must carry the artifact digest: %s", signer.statement)
	}
	if string(store.bundle) != `{"mediaType":"bundle"}` {
		t.Errorf("the signed bundle must be stored verbatim, got %s", store.bundle)
	}
}

// TestAttestWrapsFailuresAsAttestError: 失敗は attest.Error で上がる(上位が attest_failed に分類できる)。
func TestAttestWrapsFailuresAsAttestError(t *testing.T) {
	boom := errors.New("boom")
	_, err := Attest(context.Background(), ciOptions(), []Subject{{Name: "a", SHA256: "d"}},
		&fakeTokens{err: boom}, &fakeSigner{}, &fakeStore{})
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *attest.Error, got %T (%v)", err, err)
	}
	if !errors.Is(err, boom) {
		t.Error("the cause must stay reachable through Unwrap")
	}
}

// TestAttestNoSubjectsIsNoop: 証明する物が無ければ何もしない(空の証言を作らない)。
func TestAttestNoSubjectsIsNoop(t *testing.T) {
	signer := &fakeSigner{}
	res, err := Attest(context.Background(), ciOptions(), nil, &fakeTokens{}, signer, &fakeStore{})
	if err != nil {
		t.Fatalf("attest: %v", err)
	}
	if res.ID != 0 || signer.statement != nil {
		t.Fatal("nothing to attest: must not sign anything")
	}
}

// TestStatementIsSLSAProvenanceForActions: 証言の形。受け取る側(gh attestation verify)はこの形を
// 前提に「どの workflow のどの commit から出たか」を読むので、形が違えば誰も検算できない。
func TestStatementIsSLSAProvenanceForActions(t *testing.T) {
	raw, err := Statement([]Subject{{Name: "widget.tar.gz", SHA256: "abc"}}, ciOptions().Env)
	if err != nil {
		t.Fatalf("statement: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("statement is not json: %v", err)
	}
	if got["_type"] != "https://in-toto.io/Statement/v1" || got["predicateType"] != "https://slsa.dev/provenance/v1" {
		t.Fatalf("unexpected statement types: %v / %v", got["_type"], got["predicateType"])
	}
	subj := got["subject"].([]any)[0].(map[string]any)
	if subj["name"] != "widget.tar.gz" || subj["digest"].(map[string]any)["sha256"] != "abc" {
		t.Fatalf("subject lost its digest: %v", subj)
	}
	pred := got["predicate"].(map[string]any)
	run := pred["runDetails"].(map[string]any)
	builder := run["builder"].(map[string]any)["id"]
	if builder != "https://github.com/acme/widget/.github/workflows/release.yml@refs/tags/v1.2.3" {
		t.Errorf("builder.id must name the workflow that built it, got %v", builder)
	}
	def := pred["buildDefinition"].(map[string]any)
	wf := def["externalParameters"].(map[string]any)["workflow"].(map[string]any)
	if wf["path"] != ".github/workflows/release.yml" || wf["ref"] != "refs/tags/v1.2.3" {
		t.Errorf("workflow path/ref wrong: %v", wf)
	}
	dep := def["resolvedDependencies"].([]any)[0].(map[string]any)
	if dep["digest"].(map[string]any)["gitCommit"] != "abc123" {
		t.Errorf("provenance must pin the commit, not just the tag: %v", dep)
	}
}

// TestStatementRefusesWithoutWorkflow: 「誰が作ったか」を空欄にした来歴は、何も言っていないのと同じ。
func TestStatementRefusesWithoutWorkflow(t *testing.T) {
	if _, err := Statement([]Subject{{Name: "a", SHA256: "d"}}, Env{Repository: "acme/widget"}); err == nil {
		t.Fatal("a statement without a workflow/commit must be refused")
	}
}

// TestActionsTokensAsksForTheAudience: OIDC は audience 付きで取る(audience 違いは Fulcio が弾く)。
func TestActionsTokensAsksForTheAudience(t *testing.T) {
	var gotAudience, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAudience = r.URL.Query().Get("audience")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"value":"tok"}`))
	}))
	defer srv.Close()

	src := ActionsTokens{Env: OIDCEnv{RequestURL: srv.URL + "?x=1", RequestToken: "rt"}, HTTP: srv.Client()}
	tok, err := src.IDToken(context.Background(), "sigstore")
	if err != nil {
		t.Fatalf("id token: %v", err)
	}
	if tok != "tok" || gotAudience != "sigstore" || gotAuth != "Bearer rt" {
		t.Fatalf("token=%q audience=%q auth=%q", tok, gotAudience, gotAuth)
	}
}

// TestActionsTokensWithoutEnv: CI の外では取り口が無い(そこは異常ではなく前提なので、明示的に失敗する)。
func TestActionsTokensWithoutEnv(t *testing.T) {
	if _, err := (ActionsTokens{}).IDToken(context.Background(), "sigstore"); err == nil {
		t.Fatal("no OIDC endpoint: want an error")
	}
}

// TestGitHubStorePostsTheBundle: bundle は {"bundle": …} で attestations API に預ける。
func TestGitHubStorePostsTheBundle(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7}`))
	}))
	defer srv.Close()

	store := &GitHubStore{Owner: "acme", Repo: "widget", Token: "t", API: srv.URL, HTTP: srv.Client()}
	id, err := store.Put(context.Background(), []byte(`{"mediaType":"bundle"}`))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if id != 7 {
		t.Errorf("want the attestation id back, got %d", id)
	}
	if gotPath != "/repos/acme/widget/attestations" || gotAuth != "Bearer t" {
		t.Errorf("path=%q auth=%q", gotPath, gotAuth)
	}
	if string(gotBody["bundle"]) != `{"mediaType":"bundle"}` {
		t.Errorf("bundle not sent verbatim: %s", gotBody["bundle"])
	}
}

// TestGitHubStoreExplainsMissingPermission: 403 の原因はほぼ permissions 欠落——推測させず、そう言う。
func TestGitHubStoreExplainsMissingPermission(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer srv.Close()

	store := &GitHubStore{Owner: "acme", Repo: "widget", Token: "t", API: srv.URL, HTTP: srv.Client()}
	_, err := store.Put(context.Background(), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "attestations: write") {
		t.Fatalf("403 must name the missing permission, got %v", err)
	}
}

// jwtWith は payload に claims を載せただけの JWT の形。署名は付けない
// ——jobWorkflowRef は署名を検証しない(身分の真偽は Fulcio が判じる)ので、これで足りる。
func jwtWith(claims string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(claims)) + ".sig"
}

// TestAttestSignsAsTheWorkflowThatHoldsTheJob: 入口が実体を uses: で呼ぶ構成では、証明書の
// Build Signer URI は実体を指す。builder.id をそこに合わせないと GitHub は 422 で預かりを拒む。
// 一方 buildDefinition の workflow は「何が起動されたか」なので入口のままでなければならない。
func TestAttestSignsAsTheWorkflowThatHoldsTheJob(t *testing.T) {
	const reusable = "acme/widget/.github/workflows/_release.yml@refs/tags/v1.2.3"
	tokens := &fakeTokens{token: jwtWith(`{"job_workflow_ref":"` + reusable + `"}`)}
	signer := &fakeSigner{}

	if _, err := Attest(context.Background(), ciOptions(),
		[]Subject{{Name: "widget.tar.gz", SHA256: "abc"}}, tokens, signer, &fakeStore{}); err != nil {
		t.Fatalf("attest: %v", err)
	}

	pred := predicateOf(t, signer.statement)
	builder := pred["runDetails"].(map[string]any)["builder"].(map[string]any)["id"]
	if builder != "https://github.com/"+reusable {
		t.Errorf("builder.id must name the workflow that holds the job, got %v", builder)
	}
	wf := pred["buildDefinition"].(map[string]any)["externalParameters"].(map[string]any)["workflow"].(map[string]any)
	if wf["path"] != ".github/workflows/release.yml" {
		t.Errorf("externalParameters must keep naming what was triggered, got %v", wf["path"])
	}
}

// TestAttestFallsBackToTheWorkflowRef: クレームが無い/JWT の形をしていないトークンでも証明は止めない。
// 入口と実体を割っていない構成では job_workflow_ref == workflow_ref なので、落とし先が正しい。
func TestAttestFallsBackToTheWorkflowRef(t *testing.T) {
	for _, tc := range []struct{ name, token string }{
		{"no claim", jwtWith(`{"sub":"repo:acme/widget"}`)},
		{"not a jwt", "opaque-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signer := &fakeSigner{}
			if _, err := Attest(context.Background(), ciOptions(),
				[]Subject{{Name: "widget.tar.gz", SHA256: "abc"}}, &fakeTokens{token: tc.token}, signer, &fakeStore{}); err != nil {
				t.Fatalf("attest: %v", err)
			}
			pred := predicateOf(t, signer.statement)
			builder := pred["runDetails"].(map[string]any)["builder"].(map[string]any)["id"]
			if builder != "https://github.com/acme/widget/.github/workflows/release.yml@refs/tags/v1.2.3" {
				t.Errorf("builder.id must fall back to GITHUB_WORKFLOW_REF, got %v", builder)
			}
		})
	}
}

// predicateOf は署名に回った証言の predicate を取り出す。
func predicateOf(t *testing.T, statement []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(statement, &got); err != nil {
		t.Fatalf("statement is not json: %v", err)
	}
	return got["predicate"].(map[string]any)
}
