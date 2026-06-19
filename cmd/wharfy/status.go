package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// status.go — 記録(state.json)＋実体照合(Publisher.Probe)で drift を見せる(設計 04 / ADR-2)。
// status.json は Result envelope と別形(top-level に project/build/channels を持つ)。

// statusOutput は `wharfy status --json`(schemas/status.json)。
type statusOutput struct {
	SchemaVersion string           `json:"schema_version"`
	Command       string           `json:"command"`
	OK            bool             `json:"ok"`
	Message       string           `json:"message,omitempty"`
	Project       string           `json:"project"`
	Version       string           `json:"version,omitempty"`
	Tag           string           `json:"tag,omitempty"`
	Build         *statusBuild     `json:"build,omitempty"`
	Channels      []statusChannel  `json:"channels"`
	Warnings      []output.Warning `json:"warnings,omitempty"`
	Next          []output.NextDo  `json:"next"`
}

type statusBuild struct {
	OK        bool             `json:"ok"`
	Artifacts []build.Artifact `json:"artifacts,omitempty"`
}

type statusChannel struct {
	Name      string       `json:"name"`
	Kind      string       `json:"kind"`
	Published bool         `json:"published"`
	Version   string       `json:"version,omitempty"`
	Target    string       `json:"target,omitempty"`
	Reason    string       `json:"reason,omitempty"`
	Source    string       `json:"source"`
	Drift     *state.Drift `json:"drift,omitempty"`
}

// runStatus は status を組み立てて出力する(agent 同様 Result envelope と別形)。
func runStatus(ctx context.Context, asJSON bool) error {
	out, err := buildStatus(ctx, !flagNoProbe)
	if err != nil {
		return err
	}
	if asJSON {
		s, err := output.Marshal(out)
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stdout, s)
		return nil
	}
	printStatusHuman(out)
	return nil
}

// buildStatus は記録＋(probe 時)実体照合から status を組み立てる(出力と分離してテスト可能に)。
func buildStatus(ctx context.Context, probe bool) (statusOutput, error) {
	root, err := os.Getwd()
	if err != nil {
		return statusOutput{}, err
	}
	in, _ := config.Load(root)
	// main が曖昧でも status は出せる(channels は解決済み)。
	cfg, _ := config.NewResolver(root).Resolve(in)
	st, _ := state.Load(root, cfg.Project)

	tag := firstNonEmptyStr(st.LastTag, gitCurrentTag(root))
	out := statusOutput{
		SchemaVersion: output.SchemaVersion,
		Command:       "status",
		OK:            true,
		Project:       cfg.Project,
		Version:       tag,
		Tag:           tag,
		Next:          []output.NextDo{},
	}
	if st.Build != nil {
		out.Build = &statusBuild{OK: true, Artifacts: st.Build.Artifacts}
	}

	for _, ch := range cfg.Channels {
		cs, warn := assessChannel(ctx, ch, st, in, probe)
		out.Channels = append(out.Channels, cs)
		if warn != nil {
			out.Warnings = append(out.Warnings, *warn)
		}
	}
	out.Next = statusNext(out.Channels)
	out.Message = statusMessage(out.Channels)
	return out, nil
}

// assessChannel は 1 チャネルの記録＋(homebrew のみ)実体照合を行う。
func assessChannel(ctx context.Context, ch config.ResolvedChannel, st *state.State, in config.File, probe bool) (statusChannel, *output.Warning) {
	cs := statusChannel{Name: ch.Name, Kind: ch.Kind, Target: ch.Target}

	rec, hasRec := st.Publish[ch.Name]
	recordedVer := ""
	if hasRec {
		recordedVer = rec.Version
	}

	// スライス1 で実体照合できるのは homebrew のみ。他は記録のみで見せる(未実装は正直に reason)。
	if ch.Name != "homebrew" || !probe {
		cs.Source = state.SourceRecorded
		cs.Published = recordedVer != ""
		cs.Version = recordedVer
		if !cs.Published && ch.Name != "homebrew" {
			cs.Reason = "not assessed in slice 1 (homebrew only)"
		} else if !cs.Published {
			cs.Reason = "not published yet"
		}
		return cs, nil
	}

	tapOwner, tapRepo, ok := splitOwnerName(ch.Target)
	if !ok {
		cs.Source = state.SourceRecorded
		cs.Published = recordedVer != ""
		cs.Version = recordedVer
		cs.Reason = "tap unresolved — set 'github' or 'homebrew.tap'"
		return cs, nil
	}

	hb := &channel.Homebrew{
		Project: st.Project,
		Tap:     ch.Target,
		Store:   newTapStore(tapOwner, tapRepo, os.Getenv("GITHUB_TOKEN")),
	}
	rs, err := hb.Probe(ctx)
	if err != nil {
		// 照合不能は記録のみで見せ、probe_failed を warning で添える(止めない)。
		cs.Source = state.SourceRecorded
		cs.Published = recordedVer != ""
		cs.Version = recordedVer
		return cs, &output.Warning{Code: output.ErrProbeFailed, Message: "cannot probe " + ch.Target + ": " + err.Error()}
	}

	source, drift := state.Reconcile(recordedVer, rs.Version, rs.Found, true)
	cs.Source = source
	cs.Drift = drift
	cs.Published = rs.Found || recordedVer != ""
	if rs.Found {
		cs.Version = rs.Version
	} else {
		cs.Version = recordedVer
	}
	if !cs.Published {
		cs.Reason = "not published yet"
	}
	if drift != nil {
		return cs, &output.Warning{Code: output.WarnDriftDetected, Message: driftMessage(ch.Name, drift)}
	}
	return cs, nil
}

func driftMessage(name string, d *state.Drift) string {
	switch d.Kind {
	case state.DriftBehind:
		return name + " drifted: remote " + d.Remote + " behind record " + d.Recorded
	case state.DriftAhead:
		return name + " drifted: remote " + d.Remote + " ahead of record " + d.Recorded
	case state.DriftMissing:
		return name + " drifted: recorded " + d.Recorded + " but missing on remote"
	case state.DriftUntracked:
		return name + " drifted: remote " + d.Remote + " not in record (published outside wharfy)"
	}
	return name + " drifted"
}

// statusNext は drift・未発行から次の一手を組み立てる(②)。
func statusNext(channels []statusChannel) []output.NextDo {
	next := []output.NextDo{}
	for _, c := range channels {
		if c.Drift != nil {
			next = append(next, output.NextDo{
				Reason: c.Name + " drifted (" + c.Drift.Kind + ")",
				Do:     "wharfy publish " + c.Name,
			})
		}
	}
	// homebrew が未発行なら発行を促す。
	for _, c := range channels {
		if c.Name == "homebrew" && !c.Published && c.Drift == nil {
			next = append(next, output.NextDo{Reason: "homebrew not published", Do: "wharfy publish homebrew"})
		}
	}
	if len(next) == 0 {
		next = append(next, output.NextDo{Reason: "verify install works", Do: "wharfy verify"})
	}
	return next
}

func statusMessage(channels []statusChannel) string {
	drifted := 0
	for _, c := range channels {
		if c.Drift != nil {
			drifted++
		}
	}
	if drifted > 0 {
		return fmt.Sprintf("%d channel(s) drifted", drifted)
	}
	return "in sync"
}

func printStatusHuman(out statusOutput) {
	fmt.Printf("%s %s\n", out.Project, out.Version)
	if out.Build != nil {
		fmt.Printf("build: ok (%d artifacts)\n", len(out.Build.Artifacts))
	}
	fmt.Println("channels:")
	for _, c := range out.Channels {
		line := fmt.Sprintf("  %-10s %-7s", c.Name, c.Source)
		if c.Published {
			line += " published " + c.Version
		} else if c.Reason != "" {
			line += " " + c.Reason
		}
		if c.Drift != nil {
			line += fmt.Sprintf("  ⚠ drift:%s (rec:%s remote:%s)", c.Drift.Kind, c.Drift.Recorded, c.Drift.Remote)
		}
		fmt.Println(line)
	}
	if len(out.Next) > 0 {
		fmt.Println("next:")
		for _, n := range out.Next {
			fmt.Printf("  %-32s # %s\n", n.Do, n.Reason)
		}
	}
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
