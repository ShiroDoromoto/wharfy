package sign

// codesign.go — 実署名のアダプタ境界(依頼①)。build.go の Runner/LookPath と同じ思想で
// exec を注入し、テストで stub 化できるようにする(実機の証明書なしでコマンド組み立てを検証)。
// 対象は macOS の Mach-O のみ(codesign / security は macOS 専用)。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner はサブプロセス実行の差し替え点。結合出力とエラーを返す(build.Runner と同形)。
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Signer は codesign(＋P12 時の一時キーチェーン security 操作)を叩く実行器。
type Signer struct {
	Run      Runner
	LookPath func(string) (string, error)
	TempDir  string // 一時キーチェーンの置き場(空なら os.TempDir)。テスト用の差し替え点。
}

// NewSigner は本番用の既定(exec ベース)を差した Signer を返す。
func NewSigner() *Signer {
	return &Signer{Run: execRun, LookPath: exec.LookPath}
}

// UnavailableError は codesign/security(macOS 専用)が見つからない/起動不可。identity 指定済みなのに
// 署名できないのは誤設定なので、上位(release)は素通しでなく fail loud する材料にする。
type UnavailableError struct{ Err error }

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("codesign not found or not runnable: %v", e.Err)
}
func (e *UnavailableError) Unwrap() error { return e.Err }

// FailedError は署名失敗。Output は診断用(P12 パスワードは redact 済み)。
type FailedError struct {
	Err    error
	Output string
}

func (e *FailedError) Error() string { return fmt.Sprintf("sign failed: %v", e.Err) }
func (e *FailedError) Unwrap() error { return e.Err }

// SignFile は path の macOS Mach-O を opts.Identity で **in-place** 署名する。
// 呼び出し側は利用者の元バイナリではなくステージ済みコピーの path を渡すこと(元を変異させない)。
// P12 指定時は一時キーチェーンに import→署名→破棄する(グローバルのキーチェーン検索列は触らず、
// codesign --keychain で対象を限定する)。identity 未指定で呼ぶのは誤り(先に opts.Enabled を見る)。
func (s *Signer) SignFile(ctx context.Context, opts Options, path string) error {
	if opts.Identity == "" {
		return &FailedError{Err: fmt.Errorf("no signing identity")}
	}
	if _, err := s.LookPath("codesign"); err != nil {
		return &UnavailableError{Err: err}
	}

	keychain := ""
	if opts.P12 != "" {
		kc, cleanup, err := s.importP12(ctx, opts)
		if err != nil {
			return err
		}
		defer cleanup()
		keychain = kc
	}

	// --timestamp=none: オフライン/自己署名でも確実に完結させる(notarize は必須にしない方針)。
	// --force: ステージ済みコピーに既存署名があっても付け直す。
	args := []string{"--force", "--timestamp=none", "--sign", opts.Identity}
	if keychain != "" {
		args = append(args, "--keychain", keychain)
	}
	args = append(args, path)
	if out, err := s.Run(ctx, "codesign", args...); err != nil {
		return &FailedError{Err: err, Output: redact(string(out), opts.P12Pass)}
	}
	return nil
}

// importP12 は一時キーチェーンを作り P12 を import し、(キーチェーンのパス, 後始末関数) を返す。
// security(macOS 専用)が要る。set-key-partition-list で codesign が鍵を UI プロンプト無しに使えるようにする。
func (s *Signer) importP12(ctx context.Context, opts Options) (string, func(), error) {
	if _, err := s.LookPath("security"); err != nil {
		return "", func() {}, &UnavailableError{Err: err}
	}
	dir := s.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	keychain := filepath.Join(dir, "wharfy-sign-"+randHex(8)+".keychain")
	kcPass := randHex(16) // 一時キーチェーンのパス(揮発。キーチェーンは後始末で削除する)。
	cleanup := func() { _, _ = s.Run(ctx, "security", "delete-keychain", keychain) }

	steps := [][]string{
		{"create-keychain", "-p", kcPass, keychain},
		{"unlock-keychain", "-p", kcPass, keychain},
		{"import", opts.P12, "-k", keychain, "-P", opts.P12Pass, "-T", "/usr/bin/codesign", "-f", "pkcs12"},
		// partition-list には codesign: が要る(依頼書四通目=依頼①)。apple-tool:/apple: だけだと
		// /usr/bin/codesign から鍵アクセスが拒否され(errSecInternalComponent)、続く codesign --sign が
		// exit 1 で落ちる。新しめの macOS ほど partition-list に厳格でこれが露見する。
		{"set-key-partition-list", "-S", "apple-tool:,apple:,codesign:", "-s", "-k", kcPass, keychain},
	}
	for _, st := range steps {
		if out, err := s.Run(ctx, "security", st...); err != nil {
			cleanup()
			return "", func() {}, &FailedError{Err: fmt.Errorf("security %s: %w", st[0], err), Output: redact(string(out), opts.P12Pass)}
		}
	}
	return keychain, cleanup, nil
}

// redact は診断出力から P12 パスワードを伏せる(万一 security がエラーで引数を反響しても漏らさない)。
func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

// randHex は n バイトの乱数を hex 文字列で返す(一時キーチェーンの名前/パス用)。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand の失敗は事実上起きないが、起きても衝突しにくい固定値でフォールバックしない
		// (呼び出し側は毎回別名を期待)。ここでは panic せず退化名を避けるため長さ 0 を返さない。
		return "fallback"
	}
	return hex.EncodeToString(b)
}

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
