package sign

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var errNotFound = errors.New("not found")

func TestStatusReportsUnsigned(t *testing.T) {
	s := Status([]string{"linux", "darwin", "windows"}, Options{})

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
	s := Status([]string{"linux"}, Options{})
	if len(s) != 0 {
		t.Errorf("linux-only target → no signing entries, got %+v", s)
	}
}

// identity を設定すると darwin の advisory 理由が「設定済み」を示す(未設定と区別する)。
func TestStatusReflectsConfiguredIdentity(t *testing.T) {
	s := Status([]string{"darwin"}, Options{Identity: "Developer ID Application: Foo"})
	d := s["darwin"]
	if d.Signed {
		t.Error("Status does not execute signing — darwin stays signed:false until release")
	}
	if !strings.Contains(d.Reason, "identity configured") {
		t.Errorf("configured identity should be reflected in reason: %q", d.Reason)
	}
}

func TestEnabled(t *testing.T) {
	if (Options{}).Enabled() {
		t.Error("empty identity → not enabled")
	}
	if !(Options{Identity: "-"}).Enabled() {
		t.Error("any identity → enabled")
	}
}

// TestSignFileInvokesCodesign: identity で codesign が --force --sign <identity> <path> を組むこと、
// P12 無しではキーチェーン操作を挟まないこと。
func TestSignFileInvokesCodesign(t *testing.T) {
	var calls [][]string
	s := &Signer{
		LookPath: func(string) (string, error) { return "/usr/bin/codesign", nil },
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			return nil, nil
		},
	}
	if err := s.SignFile(context.Background(), Options{Identity: "Developer ID Application: Foo"}, "/dist/bin"); err != nil {
		t.Fatalf("SignFile: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected exactly one codesign call (no keychain ops without p12), got %d: %v", len(calls), calls)
	}
	got := strings.Join(calls[0], " ")
	for _, want := range []string{"codesign", "--force", "--sign", "Developer ID Application: Foo", "/dist/bin"} {
		if !strings.Contains(got, want) {
			t.Errorf("codesign call missing %q: %s", want, got)
		}
	}
}

// TestSignFileUnavailable: codesign が無ければ UnavailableError(誤設定を素通しにしないための材料)。
func TestSignFileUnavailable(t *testing.T) {
	s := &Signer{
		LookPath: func(string) (string, error) { return "", errNotFound },
		Run:      func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
	}
	err := s.SignFile(context.Background(), Options{Identity: "x"}, "/dist/bin")
	var unavail *UnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("want UnavailableError, got %v", err)
	}
}

// TestSignFileP12ImportsAndCleansUp: P12 指定時は一時キーチェーンへ create→import→codesign し、
// 成功後に delete-keychain で後始末する。codesign には --keychain が渡る。
func TestSignFileP12ImportsAndCleansUp(t *testing.T) {
	var steps []string // 実行された段(security のサブコマンド / codesign)を順に記録する。
	var codesignArgs string
	s := &Signer{
		TempDir:  t.TempDir(),
		LookPath: func(string) (string, error) { return "/usr/bin/x", nil },
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "codesign" {
				steps = append(steps, "codesign")
				codesignArgs = strings.Join(args, " ")
			} else if len(args) > 0 {
				steps = append(steps, args[0]) // security の第1引数がサブコマンド。
			}
			return nil, nil
		},
	}
	opts := Options{Identity: "Foo", P12: "/tmp/cert.p12", P12Pass: "secret"}
	if err := s.SignFile(context.Background(), opts, "/dist/bin"); err != nil {
		t.Fatalf("SignFile: %v", err)
	}
	joined := strings.Join(steps, ",")
	for _, want := range []string{"create-keychain", "import", "set-key-partition-list", "codesign", "delete-keychain"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected step %q in %v", want, steps)
		}
	}
	if steps[len(steps)-1] != "delete-keychain" {
		t.Errorf("cleanup (delete-keychain) should run last, got %v", steps)
	}
	if !strings.Contains(codesignArgs, "--keychain") {
		t.Errorf("codesign should target the temp keychain: %s", codesignArgs)
	}
}

// TestSignFileP12CleansUpOnFailure: import が失敗しても一時キーチェーンを削除する(残骸を残さない)。
func TestSignFileP12CleansUpOnFailure(t *testing.T) {
	var deleted bool
	s := &Signer{
		TempDir:  t.TempDir(),
		LookPath: func(string) (string, error) { return "/usr/bin/x", nil },
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "import" {
				return []byte("SecurityAgent: password secret was rejected"), errNotFound
			}
			if len(args) > 0 && args[0] == "delete-keychain" {
				deleted = true
			}
			return nil, nil
		},
	}
	opts := Options{Identity: "Foo", P12: "/tmp/cert.p12", P12Pass: "secret"}
	err := s.SignFile(context.Background(), opts, "/dist/bin")
	if err == nil {
		t.Fatal("expected import failure")
	}
	if !deleted {
		t.Error("temp keychain must be deleted even when import fails")
	}
	// パスワードは診断出力に漏らさない(redact)。
	var fe *FailedError
	if !errors.As(err, &fe) {
		t.Fatalf("want FailedError, got %v", err)
	}
	if strings.Contains(fe.Output, "secret") || !strings.Contains(fe.Output, "***") {
		t.Errorf("p12 password must be redacted in diagnostic output: %q", fe.Output)
	}
}
