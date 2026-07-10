package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
)

func warnCodes(ws []output.Warning) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Code)
	}
	return out
}

// 告知を載せる欄が無いチャネルは、載らなかったことを言う。
// 黙っていると配布者は「告知したつもり」で利用者を取り残す。
func TestDeprecationWarningsNoSurface(t *testing.T) {
	cfg := config.Config{Channels: []config.ResolvedChannel{
		{Name: "goinstall", Deprecated: &config.Deprecation{Ship: true, NoticeSurface: false}},
		{Name: "script", Deprecated: &config.Deprecation{Ship: true, NoticeSurface: true}},
		{Name: "homebrew"},
	}}
	ws := deprecationWarnings(cfg)
	if len(ws) != 1 {
		t.Fatalf("exactly one warning expected, got %v", warnCodes(ws))
	}
	if ws[0].Code != output.WarnDeprecateNoSurface {
		t.Errorf("code = %q", ws[0].Code)
	}
	if !strings.Contains(ws[0].Message, "goinstall") || !strings.Contains(ws[0].Message, "latest.json") {
		t.Errorf("the warning must name the channel and the surviving path: %q", ws[0].Message)
	}
}

// channels から外すと告知の更新が止まる。禁じないが、黙ってもいない。
func TestDeprecationWarningsOrphan(t *testing.T) {
	cfg := config.Config{OrphanDeprecations: []string{"aur"}}
	ws := deprecationWarnings(cfg)
	if len(ws) != 1 || ws[0].Code != output.WarnDeprecateOrphan {
		t.Fatalf("orphan warning expected, got %v", warnCodes(ws))
	}
	if !strings.Contains(ws[0].Message, "aur") {
		t.Errorf("the warning must name the channel: %q", ws[0].Message)
	}
}

// 宣言が無ければ何も言わない(既存利用者の出力を変えない)。
func TestDeprecationWarningsSilentWhenUndeclared(t *testing.T) {
	cfg := config.Config{Channels: []config.ResolvedChannel{{Name: "script"}, {Name: "homebrew"}}}
	if ws := deprecationWarnings(cfg); len(ws) != 0 {
		t.Errorf("must stay silent, got %v", warnCodes(ws))
	}
}

// deprecated を載せた status 出力が schemas/status.json に valid であること。
// status.json は config.json の resolvedChannel.deprecated へクロスファイル $ref を張るので、
// 参照が解決することもここで確かめる。
func TestStatusWithDeprecationValidatesSchema(t *testing.T) {
	out := statusOutput{
		SchemaVersion: "1",
		Command:       "status",
		OK:            true,
		Project:       "mytool",
		Channels: []statusChannel{
			{Name: "script", Kind: "owned", Published: true, Version: "1.4.0", Source: "probed",
				Deprecated: &config.Deprecation{Since: "1.4.0", Ship: false, Message: "畳みます", NoticeSurface: true}},
			{Name: "goinstall", Kind: "owned", Source: "recorded",
				Deprecated: &config.Deprecation{Since: "1.4.0", Ship: true, NoticeSurface: false}},
			{Name: "homebrew", Kind: "owned", Source: "probed"},
		},
		Warnings: deprecationWarnings(config.Config{
			Channels: []config.ResolvedChannel{
				{Name: "goinstall", Deprecated: &config.Deprecation{Ship: true, NoticeSurface: false}},
			},
			OrphanDeprecations: []string{"aur"},
		}),
		Next: []output.NextDo{},
	}
	validateAgainst(t, "https://wharfy.io/schemas/v1/status.json", out)
	if len(out.Warnings) != 2 {
		t.Errorf("expected both warnings, got %v", warnCodes(out.Warnings))
	}
}

// latest.json は「すでに入れた人」への唯一の経路。契約(schemas/latest.json)に valid であること。
// deprecations の有無どちらも通す(未宣言なら鍵ごと消える)。
func TestLatestJSONValidatesSchema(t *testing.T) {
	assets := []config.LatestAsset{{OS: "darwin", Arch: "arm64", Name: "mytool_1.4.0_darwin_arm64.tar.gz"}}
	cases := map[string]config.Config{
		"declared": {
			Project: "mytool", Github: "acme/mytool",
			Channels: []config.ResolvedChannel{
				{Name: "script", Kind: "owned", Deprecated: &config.Deprecation{Since: "1.4.0", Ship: false, Message: "畳みます", NoticeSurface: true}},
			},
		},
		"undeclared": {
			Project: "mytool", Github: "acme/mytool",
			Channels: []config.ResolvedChannel{{Name: "script", Kind: "owned"}},
		},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			content, ok := config.GenerateLatestJSON(cfg, "1.4.0", assets)
			if !ok {
				t.Fatal("latest.json must generate")
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(content), &doc); err != nil {
				t.Fatal(err)
			}
			validateAgainst(t, "https://wharfy.io/schemas/v1/latest.json", doc)
			_, has := doc["deprecations"]
			if want := name == "declared"; has != want {
				t.Errorf("deprecations present = %v, want %v\n%s", has, want, content)
			}
		})
	}
}
