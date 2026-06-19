package main

import (
	"context"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

const statusSchemaID = "https://wharfy.io/schemas/v1/status.json"

// TestStatusValidatesSchema: drift を含む status 出力が status.json に valid
// (source=drift なら drift 必須の if/then を踏む)。
func TestStatusValidatesSchema(t *testing.T) {
	out := statusOutput{
		SchemaVersion: "1",
		Command:       "status",
		OK:            true,
		Message:       "1 channel(s) drifted",
		Project:       "demo",
		Version:       "v1.4.0",
		Tag:           "v1.4.0",
		Build:         &statusBuild{OK: true},
		Channels: []statusChannel{
			{
				Name: "homebrew", Kind: "owned", Published: true, Version: "1.3.0",
				Target: "acme/homebrew-demo", Source: state.SourceDrift,
				Drift: &state.Drift{Recorded: "1.4.0", Remote: "1.3.0", Kind: state.DriftBehind},
			},
			{Name: "scoop", Kind: "owned", Published: false, Source: state.SourceRecorded, Reason: "not assessed"},
		},
		Next: []output.NextDo{{Reason: "homebrew drifted (behind)", Do: "wharfy publish homebrew"}},
	}
	validateAgainst(t, statusSchemaID, out)
}

// TestStatusDetectsDrift: 記録 v1.2.0 vs tap 上 v1.1.0 → homebrew が source=drift(behind)。
// 目印: わざと drift を起こすと status が source:"drift" で検出。
func TestStatusDetectsDrift(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)

	// 記録: homebrew を v1.2.0 で発行済みとする。
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{
		"homebrew": {Version: "1.2.0", Target: "acme/homebrew-demo", At: "t"},
	}
	if err := state.Save(root, st); err != nil {
		t.Fatal(err)
	}

	// 実体(tap): formula は古い v1.1.0。
	store := channel.NewInMemoryTapStore()
	store.Files["Formula/demo.rb"] = `class Demo < Formula
  version "1.1.0"
end
`
	defer swapTapStore(store)()

	out, err := buildStatus(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	hb := findChannel(out.Channels, "homebrew")
	if hb == nil {
		t.Fatal("homebrew channel missing")
	}
	if hb.Source != state.SourceDrift {
		t.Fatalf("source = %q, want drift", hb.Source)
	}
	if hb.Drift == nil || hb.Drift.Kind != state.DriftBehind {
		t.Fatalf("drift = %+v, want behind", hb.Drift)
	}
	if hb.Drift.Recorded != "1.2.0" || hb.Drift.Remote != "1.1.0" {
		t.Errorf("drift versions wrong: %+v", hb.Drift)
	}
	// drift があれば next: で発行を促す。
	if !hasNextDoOut(out, "wharfy publish homebrew") {
		t.Errorf("drift should drive next publish: %+v", out.Next)
	}
}

// TestStatusNoProbeIsRecorded: --no-probe 相当(probe=false)は全チャネル recorded。
func TestStatusNoProbeIsRecorded(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	st, _ := state.Load(root, "demo")
	st.Publish = map[string]state.PublishRecord{"homebrew": {Version: "1.2.0", At: "t"}}
	_ = state.Save(root, st)

	out, err := buildStatus(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range out.Channels {
		if c.Source != state.SourceRecorded {
			t.Errorf("channel %s source = %q, want recorded (no probe)", c.Name, c.Source)
		}
		if c.Drift != nil {
			t.Errorf("no-probe must not report drift: %s %+v", c.Name, c.Drift)
		}
	}
}

func findChannel(channels []statusChannel, name string) *statusChannel {
	for i := range channels {
		if channels[i].Name == name {
			return &channels[i]
		}
	}
	return nil
}

func hasNextDoOut(out statusOutput, do string) bool {
	for _, n := range out.Next {
		if n.Do == do {
			return true
		}
	}
	return false
}
