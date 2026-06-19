package channel

import (
	"context"
	"encoding/json"
	"fmt"
)

// scoop.go — Scoop Publisher(owned・即時。設計 02/03/11)。自前 bucket の
// bucket/<project>.json マニフェストを所有する。homebrew と同型: Plan で差分を見せ、
// Publish で bucket に書き、Probe で bucket 上の版を確認する(04)。
// Windows は zip アーカイブを参照する(amd64→64bit / arm64→arm64)。

// ScoopArch は 1 アーキの配布(windows)。URL は Releases の zip。
type ScoopArch struct {
	Arch   string // amd64 | arm64
	URL    string
	SHA256 string
}

// ScoopInput は manifest 生成の入力。
type ScoopInput struct {
	Project      string
	Binary       string // 既定: Project。bin は <Binary>.exe
	Description  string
	Homepage     string
	License      string
	Version      string   // 先頭 v なし(例: 1.4.0)
	Dependencies []string // ランタイム依存(manifest の "depends")。空なら出さない
	Owner, Repo  string   // autoupdate URL の Releases リポジトリ
	Archives     []ScoopArch
}

// Scoop は scoop チャネルの Publisher。Bucket は "owner/scoop-<project>"。
type Scoop struct {
	Project string
	Bucket  string
	Input   ScoopInput
	Store   TapStore
}

func (s *Scoop) Name() string { return "scoop" }
func (s *Scoop) Kind() string { return KindOwned }

// ManifestPath は bucket 内の manifest の場所(所有対象＝この path だけを書く)。
func (s *Scoop) ManifestPath() string { return "bucket/" + s.Project + ".json" }

func (s *Scoop) ownedArtifact() string { return s.Bucket + ":" + s.ManifestPath() }

// Plan は manifest を生成し、bucket 上の現状と突き合わせて操作と差分を返す(書かない)。
func (s *Scoop) Plan(ctx context.Context) (PlanItem, error) {
	want := GenerateScoopManifest(s.Input)
	base, found, err := s.Store.Get(ctx, s.ManifestPath())
	if err != nil {
		return PlanItem{}, fmt.Errorf("probe bucket manifest: %w", err)
	}
	item := PlanItem{Channel: s.Name(), Kind: s.Kind(), OwnedArtifact: s.ownedArtifact()}
	switch {
	case !found:
		item.Action = ActionCreate
		item.Diff = Diff("", want)
	case base == want:
		item.Action = ActionNoop
	default:
		item.Action = ActionUpdate
		item.Diff = Diff(base, want)
	}
	return item, nil
}

// Publish は差分があれば bucket に書く(noop なら書かない)。owned manifest のみ(03)。
func (s *Scoop) Publish(ctx context.Context) (PlanItem, PubResult, error) {
	item, err := s.Plan(ctx)
	if err != nil {
		return PlanItem{}, PubResult{}, err
	}
	if item.Action == ActionNoop {
		return item, PubResult{}, nil
	}
	want := GenerateScoopManifest(s.Input)
	msg := fmt.Sprintf("wharfy: %s %s %s", item.Action, s.Project, s.Input.Version)
	commit, err := s.Store.Put(ctx, s.ManifestPath(), want, msg)
	if err != nil {
		return item, PubResult{}, err
	}
	return item, PubResult{Commit: commit}, nil
}

// RepoExists は自前 bucket リポジトリが在るか(dry-run の予告に使う)。
func (s *Scoop) RepoExists(ctx context.Context) (bool, error) { return s.Store.Exists(ctx) }

// EnsureRepo は bucket が無ければ作る(--yes の上でのみ・ADR-8)。
func (s *Scoop) EnsureRepo(ctx context.Context) (bool, error) { return ensureRepo(ctx, s.Store) }

// Probe は bucket 上の manifest の版を返す(実体・04 の照合の基点)。
func (s *Scoop) Probe(ctx context.Context) (RemoteState, error) {
	base, found, err := s.Store.Get(ctx, s.ManifestPath())
	if err != nil {
		return RemoteState{}, err
	}
	if !found {
		return RemoteState{Found: false}, nil
	}
	return RemoteState{Version: ManifestVersion(base), Found: true}, nil
}

// --- manifest 生成 / 解析 ---

type scoopManifest struct {
	Version      string                    `json:"version"`
	Description  string                    `json:"description,omitempty"`
	Homepage     string                    `json:"homepage,omitempty"`
	License      string                    `json:"license,omitempty"`
	Depends      []string                  `json:"depends,omitempty"`
	Architecture map[string]scoopArchEntry `json:"architecture"`
	Checkver     string                    `json:"checkver,omitempty"`
	Autoupdate   *scoopAutoupdate          `json:"autoupdate,omitempty"`
}

type scoopArchEntry struct {
	URL  string `json:"url"`
	Hash string `json:"hash"`
	Bin  string `json:"bin"`
}

type scoopAutoupdate struct {
	Architecture map[string]scoopAutoArch `json:"architecture"`
}

type scoopAutoArch struct {
	URL string `json:"url"`
}

// scoopArchKey は goreleaser の arch を scoop の architecture キーに直す。
func scoopArchKey(arch string) string {
	if arch == "amd64" {
		return "64bit"
	}
	return arch // arm64
}

// GenerateScoopManifest は scoop マニフェスト(JSON)を生成する。決定的(MarshalIndent)。
func GenerateScoopManifest(in ScoopInput) string {
	binary := in.Binary
	if binary == "" {
		binary = in.Project
	}
	bin := binary + ".exe"

	arch := map[string]scoopArchEntry{}
	auto := map[string]scoopAutoArch{}
	for _, a := range in.Archives {
		key := scoopArchKey(a.Arch)
		arch[key] = scoopArchEntry{URL: a.URL, Hash: a.SHA256, Bin: bin}
		if in.Owner != "" && in.Repo != "" {
			auto[key] = scoopAutoArch{
				URL: fmt.Sprintf("https://github.com/%s/%s/releases/download/v$version/%s_$version_windows_%s.zip",
					in.Owner, in.Repo, in.Project, a.Arch),
			}
		}
	}

	var depends []string
	if len(in.Dependencies) > 0 {
		depends = sortedDeps(in.Dependencies) // 決定的に sort(02 出力契約)
	}
	m := scoopManifest{
		Version:      in.Version,
		Description:  in.Description,
		Homepage:     in.Homepage,
		License:      in.License,
		Depends:      depends,
		Architecture: arch,
		Checkver:     "github",
	}
	if len(auto) > 0 {
		m.Autoupdate = &scoopAutoupdate{Architecture: auto}
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return string(b) + "\n"
}

// ManifestVersion は scoop manifest の version を読む(Probe の版照合に使う)。
func ManifestVersion(content string) string {
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		return ""
	}
	return m.Version
}
