package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/sign"
)

// fakeSigner は codesign を実行せず、渡された path に "SIG" を追記して署名を模す
// (署名でファイル内容=ハッシュが変わる状況を作る)。呼ばれた path を記録する。
func fakeSigner(t *testing.T, signedPaths *[]string) func() {
	t.Helper()
	prev := newSigner
	s := &sign.Signer{
		LookPath: func(string) (string, error) { return "/usr/bin/codesign", nil },
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "codesign" && len(args) > 0 {
				path := args[len(args)-1]
				*signedPaths = append(*signedPaths, path)
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o755)
				if err != nil {
					return nil, err
				}
				defer f.Close()
				if _, err := f.WriteString("SIG"); err != nil {
					return nil, err
				}
			}
			return nil, nil
		},
	}
	newSigner = func() *sign.Signer { return s }
	return func() { newSigner = prev }
}

func darwinArchiveSHA(t *testing.T, root string) string {
	t.Helper()
	set, found := mustLoadArtifacts(t, root)
	if !found {
		t.Fatal("no artifacts.json")
	}
	for _, a := range set.Artifacts {
		if a.OS == "darwin" {
			return a.SHA256
		}
	}
	t.Fatal("no darwin artifact recorded")
	return ""
}

// TestReleaseSignsPrebuiltDarwinBeforeChecksum: identity を設定すると release は darwin バイナリを
// archive の前に署名し、記録される archive の sha は署名後の内容を反映する(未署名時と変わる)。
// 署名は staged コピーに対して行い、利用者の元バイナリは変異させない。linux/windows は署名しない。
func TestReleaseSignsPrebuiltDarwinBeforeChecksum(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	defer swapReleaseStore(channel.NewInMemoryReleaseStore())()
	defer func() { flagYes = false }()
	flagYes = true

	// 1) identity 無し: 署名せず、素の archive sha を得る。
	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK {
		t.Fatalf("unsigned release failed: %+v", res)
	}
	unsignedSHA := darwinArchiveSHA(t, root)

	// 2) identity あり＋署名器を注入: darwin を署名してから archive する。
	t.Setenv("WHARFY_SIGN_IDENTITY", "Developer ID Application: Foo")
	var signed []string
	defer fakeSigner(t, &signed)()

	res = runRelease(context.Background(), mustLookup(t, "release"), nil)
	if !res.OK {
		t.Fatalf("signed release failed: %+v", res)
	}
	signedSHA := darwinArchiveSHA(t, root)

	if signedSHA == unsignedSHA {
		t.Errorf("archive sha must change after signing (checksum finalized post-sign): %s", signedSHA)
	}
	// darwin のみ 1 回署名(linux/windows は対象外)。
	if len(signed) != 1 {
		t.Fatalf("exactly the darwin binary should be signed, got %v", signed)
	}
	if !strings.Contains(signed[0], filepath.Join("dist", "sign")) || !strings.Contains(signed[0], "darwin") {
		t.Errorf("signed path should be a staged darwin copy under dist/sign: %s", signed[0])
	}
	// 元バイナリは変異させない(staged コピーを署名する)。
	orig, _ := os.ReadFile(filepath.Join(root, "dist", "app-darwin-arm64"))
	if string(orig) != "macos-bin" {
		t.Errorf("source binary must not be mutated by signing, got %q", string(orig))
	}
}

// TestRunSignExecutesForPrebuiltDarwin: wharfy sign は prebuilt＋identity で実際に署名し、
// darwin を signed:true で報告し、次に release を促す(no-op を「署名した」と偽装しない対)。
func TestRunSignExecutesForPrebuiltDarwin(t *testing.T) {
	root := scratchPrebuilt(t)
	chdir(t, root)
	t.Setenv("WHARFY_SIGN_IDENTITY", "Developer ID Application: Foo")
	var signed []string
	defer fakeSigner(t, &signed)()

	res := runSign(context.Background(), mustLookup(t, "sign"), nil)
	if !res.OK {
		t.Fatalf("sign should succeed: %+v", res)
	}
	if len(signed) != 1 {
		t.Fatalf("darwin binary should be signed once, got %v", signed)
	}
	if !strings.Contains(res.Message, "signed darwin") {
		t.Errorf("message should report a real signature: %q", res.Message)
	}
	if !hasNextDo(res, "wharfy release --yes") {
		t.Errorf("after signing, next should lead to release: %+v", res.Next)
	}
	validateAgainst(t, resultSchemaID, res)
}

// TestRunSignP12WithoutIdentity: p12 だけ渡して identity が無いのは誤設定として明示する
// (codesign は誰の名で署名するか決められない)。
func TestRunSignP12WithoutIdentity(t *testing.T) {
	root := scratchPrebuilt(t)
	chdir(t, root)
	t.Setenv("WHARFY_SIGN_P12", "/tmp/cert.p12")
	t.Setenv("WHARFY_SIGN_P12_PASSWORD", "secret")

	res := runSign(context.Background(), mustLookup(t, "sign"), nil)
	if !res.OK {
		t.Fatalf("advisory, should not block: %+v", res)
	}
	if !strings.Contains(res.Message, "no identity") {
		t.Errorf("should flag p12-without-identity: %q", res.Message)
	}
	validateAgainst(t, resultSchemaID, res)
}

// TestRunSignSurfacesCodesignStderr: codesign が落ちたら、その生 stderr を errors[].detail に
// surface する(依頼書四通目=依頼③)。P12 パスワードは redact 済みで漏らさない。
func TestRunSignSurfacesCodesignStderr(t *testing.T) {
	root := scratchPrebuilt(t)
	chdir(t, root)
	if err := os.WriteFile(filepath.Join(root, "cert.p12"), []byte("p12"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WHARFY_SIGN_IDENTITY", "Developer ID Application: Foo")
	t.Setenv("WHARFY_SIGN_P12", filepath.Join(root, "cert.p12"))
	t.Setenv("WHARFY_SIGN_P12_PASSWORD", "s3cr3t")

	prev := newSigner
	s := &sign.Signer{
		TempDir:  t.TempDir(),
		LookPath: func(string) (string, error) { return "/usr/bin/x", nil },
		Run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "codesign" {
				// 下層エラー(万一 stderr にパスワードが反響しても漏らさないことも確かめる)。
				return []byte("dist/sign/...: errSecInternalComponent (pw=s3cr3t)"), os.ErrPermission
			}
			return nil, nil // security の各段(create/import/partition-list/delete)は成功
		},
	}
	newSigner = func() *sign.Signer { return s }
	defer func() { newSigner = prev }()

	res := runSign(context.Background(), mustLookup(t, "sign"), nil)
	if res.OK {
		t.Fatalf("sign must fail when codesign fails: %+v", res)
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected an error problem")
	}
	d := res.Errors[0].Detail
	if !strings.Contains(d, "errSecInternalComponent") {
		t.Errorf("detail should surface codesign stderr: %q", d)
	}
	if strings.Contains(d, "s3cr3t") {
		t.Errorf("detail must not leak the p12 password: %q", d)
	}
	validateAgainst(t, resultSchemaID, res)
}

// TestReleasePrebuiltUnavailableCodesign: identity 指定済みなのに codesign が無い(非macOS等)は
// 素通しで隠さず fail する(未署名を「署名済み」と誤認させない)。
func TestReleasePrebuiltUnavailableCodesign(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("WHARFY_SIGN_IDENTITY", "Developer ID Application: Foo")
	defer swapReleaseStore(channel.NewInMemoryReleaseStore())()
	defer func() { flagYes = false }()
	flagYes = true

	prev := newSigner
	newSigner = func() *sign.Signer {
		return &sign.Signer{
			LookPath: func(string) (string, error) { return "", os.ErrNotExist },
			Run:      func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		}
	}
	defer func() { newSigner = prev }()

	res := runRelease(context.Background(), mustLookup(t, "release"), nil)
	if res.OK {
		t.Fatalf("release must fail when signing is configured but codesign is unavailable: %+v", res)
	}
	if len(res.Errors) == 0 || res.Errors[0].Code != output.ErrSignFailed {
		t.Errorf("should classify as sign_failed (not internal): %+v", res.Errors)
	}
}
