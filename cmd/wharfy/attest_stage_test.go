package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/attest"
	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
)

// fakeAttestStore は Sigstore にも GitHub にも触らず、預かった bundle と subject を覚える。
type fakeAttestStore struct {
	bundles [][]byte
	err     error
}

func (f *fakeAttestStore) Put(_ context.Context, bundleJSON []byte) (int64, error) {
	f.bundles = append(f.bundles, bundleJSON)
	return 1, f.err
}

type recordingAttestSigner struct{ statements [][]byte }

func (r *recordingAttestSigner) SignDSSE(_ context.Context, statement []byte, _ string) ([]byte, error) {
	r.statements = append(r.statements, statement)
	return []byte(`{"mediaType":"bundle"}`), nil
}

type staticTokens struct{}

func (staticTokens) IDToken(context.Context, string) (string, error) { return "id-token", nil }

// swapAttest は attest 段の末端(署名・OIDC・預け先)をまとめて差し替える。
func swapAttest(signer attest.Signer, store attest.Store) func() {
	prevS, prevT, prevSt := newAttestSigner, newAttestTokens, newAttestStore
	newAttestSigner = func() attest.Signer { return signer }
	newAttestTokens = func(attest.OIDCEnv) attest.TokenSource { return staticTokens{} }
	newAttestStore = func(string, string, string) attest.Store { return store }
	return func() { newAttestSigner, newAttestTokens, newAttestStore = prevS, prevT, prevSt }
}

// actionsEnv は「GitHub Actions の中で、id-token: write が与えられている」環境を模す。
func actionsEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://oidc.example/token")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "rt")
	t.Setenv("GITHUB_REPOSITORY", "acme/app")
	t.Setenv("GITHUB_SHA", "cafebabe")
	t.Setenv("GITHUB_REF", "refs/tags/v0.1.0")
	t.Setenv("GITHUB_WORKFLOW_REF", "acme/app/.github/workflows/release.yml@refs/tags/v0.1.0")
	t.Setenv("GITHUB_RUN_ID", "9")
	t.Setenv("GITHUB_RUN_ATTEMPT", "1")
}

// laptopEnv は「Actions の外(手元)」を模す。テスト自身が走る場所も CI なので、runner の
// GITHUB_ACTIONS / OIDC がそのまま見えてしまう——手元を名乗るなら、その env は自分で落とす。
// 落とさないと手元では通り CI でだけ赤くなる(逆も然り)。
func laptopEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
}

// TestReleaseAttestsInActions: CI で release すると、上げた成果物の digest に来歴が付く。
func TestReleaseAttestsInActions(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	actionsEnv(t)

	store := channel.NewInMemoryReleaseStore()
	defer swapReleaseStore(store)()
	signer := &recordingAttestSigner{}
	att := &fakeAttestStore{}
	defer swapAttest(signer, att)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if len(att.bundles) != 1 {
		t.Fatalf("want exactly one attestation stored, got %d", len(att.bundles))
	}
	if len(signer.statements) != 1 {
		t.Fatalf("want one signed statement, got %d", len(signer.statements))
	}
	// 証言は「配ったバイト列」を指す: release が確定した実 sha256 が subject に載る。
	data, ok := res.Data.(releaseData)
	if !ok || data.Attest == nil || len(data.Attest.Subjects) == 0 {
		t.Fatalf("release must report what it attested: %+v", res.Data)
	}
	stmt := string(signer.statements[0])
	for _, a := range data.Artifacts {
		if a.SHA256 != "" && !strings.Contains(stmt, a.SHA256) {
			t.Errorf("artifact %s (%s) is not in the provenance statement", a.Path, a.SHA256)
		}
	}
	if !strings.Contains(res.Message, "provenance") {
		t.Errorf("a release that attested should say so: %q", res.Message)
	}
}

// TestReleaseAttestsTheInstallersAndLatestJSON: release が上げるのはビルド成果物だけではない。
// install.sh は利用者が `curl | sh` で実行する物、latest.json は更新チェックの向き先 —— 供給網として
// はアーカイブより重いのに、ビルド出力ではないという理由で証明の外に置かれていた。
func TestReleaseAttestsTheInstallersAndLatestJSON(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	actionsEnv(t)

	defer swapReleaseStore(channel.NewInMemoryReleaseStore())()
	signer := &recordingAttestSigner{}
	defer swapAttest(signer, &fakeAttestStore{})()
	defer func() { flagYes = false }()
	flagYes = true

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if len(signer.statements) != 1 {
		t.Fatalf("want one signed statement, got %d", len(signer.statements))
	}
	stmt := string(signer.statements[0])
	// 名前だけでなく digest まで見る: 証言が指すのは「上げたファイルのバイト列」でなければ、
	// 受け取る側は自分の手元のファイルで検算できない。
	for _, rel := range []string{config.InstallScriptRelPath, config.InstallPS1RelPath, config.LatestJSONRelPath} {
		sum, err := build.SHA256File(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("release should have written %s: %v", rel, err)
		}
		if !strings.Contains(stmt, sum) {
			t.Errorf("%s is uploaded to the release, so its digest belongs in the provenance statement", rel)
		}
	}
}

// TestReleaseInActionsWithoutPermissionsWarns: CI なのに証明できないなら、黙って配らず警告する
// (証明は無くても配れてしまうので、黙ると「付いているつもり」のまま出続ける)。
func TestReleaseInActionsWithoutPermissionsWarns(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	laptopEnv(t)
	t.Setenv("GITHUB_ACTIONS", "true") // id-token: write が無い＝OIDC の取り口が無い

	store := channel.NewInMemoryReleaseStore()
	defer swapReleaseStore(store)()
	att := &fakeAttestStore{}
	defer swapAttest(&recordingAttestSigner{}, att)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK {
		t.Fatalf("the release itself still succeeds: %+v", res)
	}
	if len(att.bundles) != 0 {
		t.Fatal("nothing can be attested without OIDC")
	}
	if !hasWarning(res, output.WarnAttestUnavailable) {
		t.Fatalf("want %s warning, got %+v", output.WarnAttestUnavailable, res.Warnings)
	}
}

// TestReleaseOutsideActionsAttestsNothingQuietly: 手元では証明できないのが前提。警告もしない。
func TestReleaseOutsideActionsAttestsNothingQuietly(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	laptopEnv(t)

	store := channel.NewInMemoryReleaseStore()
	defer swapReleaseStore(store)()
	att := &fakeAttestStore{}
	defer swapAttest(&recordingAttestSigner{}, att)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if len(att.bundles) != 0 {
		t.Fatal("a laptop has no OIDC identity: nothing may be attested")
	}
	if hasWarning(res, output.WarnAttestUnavailable) {
		t.Error("not being in CI is the premise, not a warning")
	}
}

// TestReleaseFailsWhenAttestFails: 証明できる環境で証明に失敗したら release ごと赤くする
// (証明の無いリリースを緑で通さない)。
func TestReleaseFailsWhenAttestFails(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	actionsEnv(t)

	store := channel.NewInMemoryReleaseStore()
	defer swapReleaseStore(store)()
	defer swapAttest(&recordingAttestSigner{}, &fakeAttestStore{err: errors.New("403")})()
	defer func() { flagYes = false }()
	flagYes = true

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if res.OK {
		t.Fatal("an unattested release must not be reported green")
	}
	if len(res.Errors) == 0 || res.Errors[0].Code != output.ErrAttestFailed {
		t.Fatalf("want %s, got %+v", output.ErrAttestFailed, res.Errors)
	}
}
