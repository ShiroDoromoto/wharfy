package main

// sign_stage.go — sign 段の CLI 側オーケストレーション(依頼①)。yaml/env から署名設定を解決し、
// prebuilt の macOS Mach-O を「ステージ(コピー)→ codesign → checksum」の順で署名する。
// release はこれを ArchivePrebuilt の**前**に呼ぶので、archive の checksum は署名後の実体を反映する。
// 利用者の元バイナリは変異させない(必ずコピーを署名する)。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/sign"
)

// newSigner は Signer の生成点(テストで差し替える＝末端は差し替え可能)。
var newSigner = func() *sign.Signer { return sign.NewSigner() }

// 署名設定の env フォールバック。秘密(パスワード)は env のみ受け付け、yaml/生成物には出さない。
const (
	envSignIdentity    = "WHARFY_SIGN_IDENTITY"
	envSignP12         = "WHARFY_SIGN_P12"
	envSignP12Password = "WHARFY_SIGN_P12_PASSWORD"
)

// resolveSignOptions は yaml(sign:)と env から署名設定を組み立てる。
// identity は yaml 優先・空なら env。P12 パス同様。パスワードは env のみ。
func resolveSignOptions(in config.File) sign.Options {
	var id, p12 string
	if in.Sign != nil {
		id = in.Sign.Identity
		p12 = in.Sign.P12
	}
	if id == "" {
		id = os.Getenv(envSignIdentity)
	}
	if p12 == "" {
		p12 = os.Getenv(envSignP12)
	}
	return sign.Options{Identity: id, P12: p12, P12Pass: os.Getenv(envSignP12Password)}
}

// signedBinary は署名した 1 バイナリの結果(wharfy sign の報告・release の記録用)。
type signedBinary struct {
	OS     string
	Arch   string
	Path   string // distDir 相対の署名済みステージファイル
	SHA256 string // 署名後のバイナリ自身の sha256(配布 checksum は archive 側で確定する)
}

// stageSignDarwin は darwin バイナリをステージ(コピー)して署名し、Path を署名済みコピーへ差し替えた
// バイナリ列を返す。非 darwin は素通し。opts.Enabled() 済み前提で呼ぶ。
// 署名済みコピーは <distDir>/sign/ に置く(元バイナリは触らない)。
func stageSignDarwin(ctx context.Context, root, distDir string, opts sign.Options, bins []build.PrebuiltBinary) ([]build.PrebuiltBinary, []signedBinary, error) {
	signer := newSigner()
	stageDir := filepath.Join(root, distDir, "sign")

	out := make([]build.PrebuiltBinary, 0, len(bins))
	var signed []signedBinary
	for _, bin := range bins {
		if bin.OS != "darwin" {
			out = append(out, bin) // Windows Authenticode / linux は対象外(将来)。
			continue
		}
		src := bin.Path
		if !filepath.IsAbs(src) {
			src = filepath.Join(root, bin.Path)
		}
		if err := os.MkdirAll(stageDir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("mkdir sign stage: %w", err)
		}
		dst := filepath.Join(stageDir, bin.OS+"_"+bin.Arch+"_"+filepath.Base(bin.Path))
		if err := copyFile(src, dst); err != nil {
			return nil, nil, fmt.Errorf("stage %s: %w", bin.Path, err)
		}
		if err := signer.SignFile(ctx, opts, dst); err != nil {
			return nil, nil, err // UnavailableError / FailedError はそのまま上げて fail loud にする。
		}
		sum, err := sha256File(dst)
		if err != nil {
			return nil, nil, fmt.Errorf("checksum signed %s: %w", bin.Path, err)
		}
		rel, err := filepath.Rel(root, dst)
		if err != nil {
			rel = dst
		}
		nb := bin
		nb.Path = rel
		nb.SHA256 = "" // 署名でハッシュが変わったので宣言 sha は破棄(archive 側が実 sha を確定する)。
		out = append(out, nb)
		signed = append(signed, signedBinary{OS: bin.OS, Arch: bin.Arch, Path: rel, SHA256: sum})
	}
	return out, signed, nil
}

// signPrebuiltBinaries は release/sign 共通の入口。署名が有効かつ prebuilt に darwin があれば署名し、
// 差し替え済みバイナリ列と署名結果を返す。無効なら bins を素通しで返す(signed=nil)。
func signPrebuiltBinaries(ctx context.Context, root string, opts sign.Options, bins []build.PrebuiltBinary) ([]build.PrebuiltBinary, []signedBinary, error) {
	if !opts.Enabled() || !hasDarwin(bins) {
		return bins, nil, nil
	}
	return stageSignDarwin(ctx, root, config.DistDir, opts, bins)
}

func hasDarwin(bins []build.PrebuiltBinary) bool {
	for _, b := range bins {
		if b.OS == "darwin" {
			return true
		}
	}
	return false
}

// copyFile は src を dst にコピーする(mode は 0755=実行可能を保つ)。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
