// Package state はハイブリッド状態の「ローカル記録」側(設計 04 / ADR-2)。
//
// wharfy が「何をしたか」を .wharfy/state.json に逐次記録する。これは速い基点であって
// 真実ではない(drift しうる)。status は必ず実体(Publisher.Probe)と突き合わせてから提示する。
// 記録は壊れても捨てて作り直せる前提(書き込みはアトミック)。
package state

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/ShiroDoromoto/wharfy/internal/build"
)

const schemaVersion = "1"

// FileName は記録の置き場所(.wharfy/ 配下＝wharfy のスクラッチ・03)。
const FileName = "state.json"

// DirName は .wharfy(config.WharfyDirName と一致。循環 import を避けるため再掲)。
const DirName = ".wharfy"

// State は .wharfy/state.json の全体(04 の記録スキーマ)。
type State struct {
	SchemaVersion string                   `json:"schema_version"`
	Project       string                   `json:"project"`
	LastTag       string                   `json:"last_tag,omitempty"`
	Build         *BuildRecord             `json:"build,omitempty"`
	Publish       map[string]PublishRecord `json:"publish,omitempty"`
}

// BuildRecord は最後の build の記録。
type BuildRecord struct {
	Tag       string           `json:"tag,omitempty"`
	At        string           `json:"at"`
	Artifacts []build.Artifact `json:"artifacts"`
}

// PublishRecord はチャネルへの発行記録(status の照合の基点)。
// gated(winget 等)は State / PR で申請の進行を追う(11A)。
type PublishRecord struct {
	Version string `json:"version,omitempty"`
	Target  string `json:"target,omitempty"`
	Commit  string `json:"commit,omitempty"`
	State   string `json:"state,omitempty"` // gated: none|prepared|pr_open|merged|closed|rejected
	PR      string `json:"pr,omitempty"`    // gated: 申請 PR の URL
	At      string `json:"at"`
}

// Path は root 配下の state.json のパス。
func Path(root string) string {
	return filepath.Join(root, DirName, FileName)
}

// Load は記録を読む。無ければ空 State(エラーではない＝記録は最適化・実体が真実)。
// 壊れている場合はエラーを返す(呼び出し側はフォールバックして作り直せる・04)。
func Load(root, project string) (*State, error) {
	b, err := os.ReadFile(Path(root))
	if os.IsNotExist(err) {
		return &State{SchemaVersion: schemaVersion, Project: project}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.SchemaVersion == "" {
		s.SchemaVersion = schemaVersion
	}
	if s.Project == "" {
		s.Project = project
	}
	return &s, nil
}

// Save は記録をアトミックに書く(temp に書いて rename。途中失敗で state.json を壊さない・04)。
func Save(root string, s *State) error {
	if s.SchemaVersion == "" {
		s.SchemaVersion = schemaVersion
	}
	dir := filepath.Join(root, DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功時は存在しないので no-op
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, Path(root))
}

// RecordBuild は build の結果を記録に反映する(呼び出し側が Save する)。
func (s *State) RecordBuild(tag, at string, artifacts []build.Artifact) {
	s.Build = &BuildRecord{Tag: tag, At: at, Artifacts: artifacts}
	if tag != "" {
		s.LastTag = tag
	}
}
