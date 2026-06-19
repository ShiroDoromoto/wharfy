// Package registry は能力の単一真実(設計 01 能力レジストリ / 05 drift 対策)。
//
// コマンド・要約・引数・既定 next: をここ 1 か所に持つ。cobra のコマンド登録も
// agent の一枚出力も「ここから生成」する。手書きの能力一覧は実体とズレるため持たない。
// 新コマンドはここに足すだけで agent 出力・補完・docs に自動で載る。
//
// 依存なし(純データ)。下位層として上位を知らない(依存は上から下への一方向)。
package registry

// Command はレジストリの 1 エントリ。schemas/common.json の commandSpec と同形。
// agent --json の commands[] はこれの配列(02 出力契約)。
type Command struct {
	Name    string   `json:"name"`
	Summary string   `json:"summary"`
	Args    string   `json:"args,omitempty"`
	Next    []string `json:"next,omitempty"` // 既定の次コマンド名。参照先は必ず registry に実在する(05)。
}

// ChannelRef は agent 出力でのチャネル参照(名前と種別のみ)。schemas/common.json の channelRef。
type ChannelRef struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // owned | gated (common.json channelKind)
}

// AgentDoc は `wharfy agent --json` の出力(schemas/agent.json)。Result envelope とは別形。
// registry から生成するので実体とズレない(05)。
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

// Commands は唯一の真実。順番は「通常の操作順」(02 の COMMANDS usual order)。
//
// status/build/sign/publish/verify はドメインコマンド。agent/config/version も
// cobra に登録される実コマンドなので、cobra==registry を例外なく成り立たせるため
// ここに含める(05 の「cobra にあるが registry にない」を構造的にゼロにする)。
var Commands = []Command{
	{Name: "agent", Summary: "print this capability map (read once, then drive)", Next: []string{"status"}},
	{Name: "status", Summary: "what is built / signed / published, and where", Next: []string{"build"}},
	{Name: "config", Summary: "show the resolved effective config", Next: []string{"build"}},
	{Name: "build", Summary: "cross-compile for every os/arch", Next: []string{"sign", "publish"}},
	{Name: "sign", Summary: "sign & notarize what can be signed", Next: []string{"publish"}},
	{Name: "publish", Summary: "push to owned channels; prepare gated ones", Args: "[channel]", Next: []string{"verify"}},
	{Name: "verify", Summary: "install from each channel and run it"},
	{Name: "version", Summary: "print wharfy's own version (not your project's)", Next: []string{"agent"}},
}

// StateReaders は状態の読み口になるコマンド名(02 agent の state_readers)。
var StateReaders = []string{"status", "config"}

// Channels は agent が宣伝するチャネル一覧。実装済み Publisher に追従させる(同じ生成思想)。
// homebrew で型を固めた後、低摩擦な goinstall / script から横展開中(08 §5)。
var Channels = []ChannelRef{
	{Name: "homebrew", Kind: "owned"},
	{Name: "scoop", Kind: "owned"},
	{Name: "apt", Kind: "owned"},
	{Name: "rpm", Kind: "owned"},
	{Name: "container", Kind: "owned"},
	{Name: "goinstall", Kind: "owned"},
	{Name: "script", Kind: "owned"},
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
