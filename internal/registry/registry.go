// Package registry は能力の単一真実(能力レジストリ / drift 対策)。
//
// コマンド・要約・引数・既定 next: をここ 1 か所に持つ。cobra のコマンド登録も
// agent の一枚出力も「ここから生成」する。手書きの能力一覧は実体とズレるため持たない。
// 新コマンドはここに足すだけで agent 出力・補完・docs に自動で載る。
//
// 依存なし(純データ + 整形のみ)。下位層として上位を知らない(依存は上から下への一方向)。
package registry

import (
	"fmt"
	"strings"
)

// Command はレジストリの 1 エントリ。schemas/common.json の commandSpec と同形。
// agent --json の commands[] はこれの配列(出力契約)。
type Command struct {
	Name    string   `json:"name"`
	Summary string   `json:"summary"`
	Args    string   `json:"args,omitempty"`
	Next    []string `json:"next,omitempty"` // 既定の次コマンド名。参照先は必ず registry に実在する。
}

// ChannelRef は agent 出力でのチャネル参照。schemas/common.json の channelRef。
// Notes はそのチャネルを駆動する前に知っておくべき注記(1 行 1 点)。
type ChannelRef struct {
	Name  string   `json:"name"`
	Kind  string   `json:"kind"` // owned | gated (common.json channelKind)
	Notes []string `json:"notes,omitempty"`
}

// InstallExitCode は install.sh / install.ps1 が名乗る終了コードと、その意味。
//
// wharfy が所有する生成物の対外契約なので、ここが単一真実になる。生成物のヘッダ・
// agent 出力・README で別々に書けば必ずずれる(drift 対策)。
type InstallExitCode struct {
	Code    int
	Meaning string
}

// InstallExitCodes は script チャネルの生成物が名乗る終了コード。
//
// 3 つだけ意味を持たせ、残りは全部 1 に閉じ込める。閉じ込めないと `set -e` が素通しした
// 他コマンドの終了コード(tar は致命的エラーで 2 を返す)が意味ありげな値として漏れ、
// 傍らの coding agent はそれを見て誤った次の一手を選ぶ。
var InstallExitCodes = []InstallExitCode{
	{0, "installed"},
	{1, "unexpected failure — unclassified, please report"},
	{2, "unsupported platform (os/arch)"},
	{3, "download failed (dns / tls / proxy / http error / missing asset)"},
	{4, "cannot write to the install prefix (permission, read-only fs, no space)"},
}

// InstallExitCodeLine は終了コード規約の 1 行要約(agent の notes 用)。
func InstallExitCodeLine() string {
	parts := make([]string, 0, len(InstallExitCodes))
	for _, e := range InstallExitCodes {
		parts = append(parts, fmt.Sprintf("%d=%s", e.Code, e.Meaning))
	}
	return "install.sh / install.ps1 exit codes: " + strings.Join(parts, "; ")
}

// AgentDoc は `wharfy agent --json` の出力(schemas/agent.json)。Result envelope とは別形。
// registry から生成するので実体とズレない。
type AgentDoc struct {
	SchemaVersion string       `json:"schema_version"`
	Tool          string       `json:"tool"`
	Version       string       `json:"version"`
	Start         string       `json:"start"`
	StateReaders  []string     `json:"state_readers,omitempty"`
	Commands      []Command    `json:"commands"`
	Channels      []ChannelRef `json:"channels,omitempty"`
}

const schemaVersion = "1"

// Commands は唯一の真実。順番は「通常の操作順」。
//
// status/build/sign/publish/verify はドメインコマンド。agent/config/version も
// cobra に登録される実コマンドなので、cobra==registry を例外なく成り立たせるため
// ここに含める(「cobra にあるが registry にない」を構造的にゼロにする)。
var Commands = []Command{
	{Name: "agent", Summary: "print this capability map (read once, then drive)", Next: []string{"status"}},
	{Name: "status", Summary: "what is built / signed / published, and where", Next: []string{"build"}},
	{Name: "config", Summary: "show the resolved effective config", Next: []string{"build"}},
	{Name: "auth", Summary: "save a credential (e.g. fury token) to the OS keychain", Args: "<kind>", Next: []string{"publish"}},
	{Name: "init", Summary: "tell agents to release via wharfy (writes AGENTS.md / CLAUDE.md)", Next: []string{"agent"}},
	{Name: "build", Summary: "cross-compile for every os/arch", Next: []string{"sign", "release"}},
	{Name: "sign", Summary: "codesign macOS binaries with your identity (opt-in; skipped if none)", Next: []string{"release"}},
	{Name: "release", Summary: "upload the github release (archives, packages, install.sh, install.ps1, latest.json)", Next: []string{"publish"}},
	{Name: "publish", Summary: "push to owned channels; prepare gated ones", Args: "[channel]", Next: []string{"verify"}},
	{Name: "verify", Summary: "check each channel from the consumer side (--install: install from it and run it)", Args: "[channel]"},
	{Name: "version", Summary: "print wharfy's own version (not your project's)", Next: []string{"agent"}},
}

// StateReaders は状態の読み口になるコマンド名(agent の state_readers)。
var StateReaders = []string{"status", "config"}

// Channels は agent が宣伝するチャネル一覧。実装済み Publisher に追従させる(同じ生成思想)。
// homebrew で型を固めた後、低摩擦な goinstall / script から横展開中(§5)。
var Channels = []ChannelRef{
	{Name: "homebrew", Kind: "owned"},
	{Name: "cask", Kind: "owned"},
	{Name: "scoop", Kind: "owned"},
	{Name: "apt", Kind: "owned"},
	{Name: "rpm", Kind: "owned"},
	{Name: "container", Kind: "owned"},
	{Name: "aur", Kind: "owned"},
	{Name: "goinstall", Kind: "owned"},
	{Name: "script", Kind: "owned", Notes: []string{
		"on failure the scripts print the cause and the next move; they never retry, never fall back to a mirror, and never elevate",
		InstallExitCodeLine(),
	}},
	{Name: "winget", Kind: "gated"},
	{Name: "homebrew-core", Kind: "gated"},
}

// Names は登録コマンド名の集合。drift テストや next: 参照健全性の照合に使う。
func Names() map[string]bool {
	m := make(map[string]bool, len(Commands))
	for _, c := range Commands {
		m[c.Name] = true
	}
	return m
}

// Lookup は名前でコマンドを引く。無ければ ok=false。
func Lookup(name string) (Command, bool) {
	for _, c := range Commands {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

// BuildAgentDoc は registry から agent 出力を生成する(①聞けば分かる)。
// version は cmd 層の versionLine() を渡す(version 注入は package main 側)。
func BuildAgentDoc(version string) AgentDoc {
	return AgentDoc{
		SchemaVersion: schemaVersion,
		Tool:          "wharfy",
		Version:       version,
		Start:         "wharfy status",
		StateReaders:  StateReaders,
		Commands:      Commands,
		Channels:      Channels,
	}
}
