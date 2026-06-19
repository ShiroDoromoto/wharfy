// Package build はクロスビルドのアダプタ境界(設計 01 Builder / ADR-1・ADR-5)。
//
// 上位層は Builder インタフェースしか知らない。GoReleaser 依存はこのパッケージに閉じ、
// (A)→(C) 独立移行時は nativeBuilder を実装して差し替えるだけにする。
// ADR-5 によりライブラリ import せず、pin したバイナリをサブプロセスで叩く。
package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// Artifact はクロスビルドの成果物 1 つ(schemas/common.json の artifact と同形)。
type Artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
}

// Builder はビルド境界。configPath は生成 GoReleaser 設定(.wharfy/goreleaser.yaml)。
// root は利用者リポジトリのルート(サブプロセスの作業ディレクトリ)。
type Builder interface {
	Build(ctx context.Context, root, configPath string) ([]Artifact, error)
}

// UnavailableError は下層ビルダが見つからない/起動不可(09 builder_unavailable)。
type UnavailableError struct {
	Bin string
	Err error
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("%s not found or not runnable: %v", e.Bin, e.Err)
}
func (e *UnavailableError) Unwrap() error { return e.Err }

// FailedError はクロスビルド失敗(09 build_failed)。Output は診断のための末尾ログ。
type FailedError struct {
	Err    error
	Output string
}

func (e *FailedError) Error() string { return fmt.Sprintf("build failed: %v", e.Err) }
func (e *FailedError) Unwrap() error { return e.Err }

// Runner はサブプロセス実行の差し替え点(テストで stub 化する＝末端は差し替え可能・01)。
// 結合出力とエラーを返す。
type Runner func(ctx context.Context, dir, name string, args ...string) ([]byte, error)

// GoReleaserBuilder は MVP 実装。GoReleaser を `build --snapshot` でサブプロセス起動する。
type GoReleaserBuilder struct {
	Bin      string // 既定 "goreleaser"
	DistDir  string // root からの相対(生成設定の dist と一致させる)
	LookPath func(string) (string, error)
	Run      Runner
}

// NewGoReleaserBuilder は本番用の既定(exec ベース)を差した Builder を返す。
// distDir は config.DistDir を渡す(生成設定の dist と一致させるため呼び出し側で指定)。
func NewGoReleaserBuilder(distDir string) *GoReleaserBuilder {
	return &GoReleaserBuilder{
		Bin:      "goreleaser",
		DistDir:  distDir,
		LookPath: exec.LookPath,
		Run:      execRun,
	}
}

func (b *GoReleaserBuilder) Build(ctx context.Context, root, configPath string) ([]Artifact, error) {
	if _, err := b.LookPath(b.Bin); err != nil {
		return nil, &UnavailableError{Bin: b.Bin, Err: err}
	}
	// build = クロスビルドのみ(発行しない)。--snapshot で tag 無しでも通す。--clean で dist を掃除。
	out, err := b.Run(ctx, root, b.Bin, "build", "--snapshot", "--clean", "--config", configPath)
	if err != nil {
		return nil, &FailedError{Err: err, Output: tail(out, 4000)}
	}
	return parseArtifacts(root, filepath.Join(root, b.DistDir, "artifacts.json"))
}

// glArtifact は GoReleaser の dist/artifacts.json の 1 エントリ(必要分のみ)。
type glArtifact struct {
	Path string `json:"path"`
	GOOS string `json:"goos"`
	Arch string `json:"goarch"`
	Type string `json:"type"`
}

// parseArtifacts は artifacts.json を読み、Binary だけ抜き、各ファイルの sha256 を自前計算する。
// GoReleaser のチェックサム形式に依存しないため、実ファイルから求める(robust)。
func parseArtifacts(root, artifactsPath string) ([]Artifact, error) {
	b, err := os.ReadFile(artifactsPath)
	if err != nil {
		return nil, &FailedError{Err: fmt.Errorf("read %s: %w", artifactsPath, err)}
	}
	var raw []glArtifact
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, &FailedError{Err: fmt.Errorf("parse artifacts.json: %w", err)}
	}
	var out []Artifact
	for _, a := range raw {
		if a.Type != "Binary" {
			continue
		}
		full := a.Path
		if !filepath.IsAbs(full) {
			full = filepath.Join(root, a.Path)
		}
		sum, err := sha256File(full)
		if err != nil {
			return nil, &FailedError{Err: fmt.Errorf("checksum %s: %w", a.Path, err)}
		}
		out = append(out, Artifact{OS: a.GOOS, Arch: a.Arch, Path: a.Path, SHA256: sum})
	}
	return out, nil
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

func execRun(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "…" + string(b[len(b)-n:])
}
