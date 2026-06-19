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
	State     string       `json:"state,omitempty"` // gated の申請状態(11A)
	PR        string       `json:"pr,omitempty"`    // gated の PR URL
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
		cs, warn := assessChannel(ctx, ch, cfg, st, probe, tag)
		out.Channels = append(out.Channels, cs)
		if warn != nil {
			out.Warnings = append(out.Warnings, *warn)
		}
	}
	out.Next = statusNext(out.Channels)
	out.Message = statusMessage(out.Channels)
	return out, nil
}

// assessChannel は 1 チャネルの記録＋実体照合を行う。実装済み(homebrew/script/goinstall)は
// 各チャネルの実体を probe し、未実装は記録のみで見せる(③ 黙って合わせず source/drift で見せる)。
func assessChannel(ctx context.Context, ch config.ResolvedChannel, cfg config.Config, st *state.State, probe bool, tag string) (statusChannel, *output.Warning) {
	cs := statusChannel{Name: ch.Name, Kind: ch.Kind, Target: ch.Target}
	recordedVer := st.Publish[ch.Name].Version

	if !probe {
		return recordedOnly(cs, recordedVer, "not published yet"), nil
	}

	switch ch.Name {
	case "homebrew":
		return assessHomebrew(ctx, cs, ch, cfg.Project, recordedVer)
	case "scoop":
		return assessScoop(ctx, cs, ch, cfg.Project, recordedVer)
	case "script":
		return assessScript(ctx, cs, cfg, recordedVer)
	case "goinstall":
		return assessGoinstall(ctx, cs, ch.Target, tag)
	case "winget":
		return assessWinget(cs, st.Publish["winget"])
	default:
		return recordedOnly(cs, recordedVer, "not assessed yet (no probe for this channel)"), nil
	}
}

// assessWinget は gated の申請状態を記録から見せる(none/prepared/pr_open/merged/...・11A)。
// PR 状態の API probe は未実装(記録ベース)。pr_open は gated_pending で注意喚起。
func assessWinget(cs statusChannel, rec state.PublishRecord) (statusChannel, *output.Warning) {
	cs.Source = state.SourceRecorded
	if rec.Version == "" {
		cs.State = "none"
		cs.Published = false
		cs.Reason = "not submitted"
		return cs, nil
	}
	cs.Version = rec.Version
	cs.State = rec.State
	cs.PR = rec.PR
	cs.Published = rec.State == "merged"
	if rec.State == "pr_open" {
		return cs, &output.Warning{Code: output.WarnGatedPending, Message: "winget PR awaiting review: " + rec.PR}
	}
	return cs, nil
}

// recordedOnly は照合せず記録のみで埋める(未 probe / 未実装チャネル)。
func recordedOnly(cs statusChannel, recordedVer, unpublishedReason string) statusChannel {
	cs.Source = state.SourceRecorded
	cs.Published = recordedVer != ""
	cs.Version = recordedVer
	if !cs.Published {
		cs.Reason = unpublishedReason
	}
	return cs
}

// reconcileInto は記録 vs 実体を照合して cs を埋め、drift なら warning を返す(homebrew/script 共通)。
func reconcileInto(cs *statusChannel, name, recordedVer string, rs channel.RemoteState) *output.Warning {
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
		return &output.Warning{Code: output.WarnDriftDetected, Message: driftMessage(name, drift)}
	}
	return nil
}

func probeFailedWarning(target string, err error) *output.Warning {
	return &output.Warning{Code: output.ErrProbeFailed, Message: "cannot probe " + target + ": " + err.Error()}
}

// assessHomebrew は自前 tap の formula 版を照合する。
func assessHomebrew(ctx context.Context, cs statusChannel, ch config.ResolvedChannel, project, recordedVer string) (statusChannel, *output.Warning) {
	tapOwner, tapRepo, ok := splitOwnerName(ch.Target)
	if !ok {
		cs = recordedOnly(cs, recordedVer, "tap unresolved — set 'github' or 'homebrew.tap'")
		return cs, nil
	}
	hb := &channel.Homebrew{Project: project, Tap: ch.Target, Store: newTapStore(tapOwner, tapRepo, os.Getenv("GITHUB_TOKEN"))}
	rs, err := hb.Probe(ctx)
	if err != nil {
		return recordedOnly(cs, recordedVer, "not published yet"), probeFailedWarning(ch.Target, err)
	}
	warn := reconcileInto(&cs, "homebrew", recordedVer, rs)
	return cs, warn
}

// assessScoop は自前 bucket の manifest 版を照合する(homebrew と同型)。
func assessScoop(ctx context.Context, cs statusChannel, ch config.ResolvedChannel, project, recordedVer string) (statusChannel, *output.Warning) {
	bOwner, bRepo, ok := splitOwnerName(ch.Target)
	if !ok {
		return recordedOnly(cs, recordedVer, "bucket unresolved — set 'github' or 'scoop.bucket'"), nil
	}
	sc := &channel.Scoop{Project: project, Bucket: ch.Target, Store: newTapStore(bOwner, bRepo, os.Getenv("GITHUB_TOKEN"))}
	rs, err := sc.Probe(ctx)
	if err != nil {
		return recordedOnly(cs, recordedVer, "not published yet"), probeFailedWarning(ch.Target, err)
	}
	warn := reconcileInto(&cs, "scoop", recordedVer, rs)
	return cs, warn
}

// assessScript は Release 上の install.sh が指す版を照合する。
func assessScript(ctx context.Context, cs statusChannel, cfg config.Config, recordedVer string) (statusChannel, *output.Warning) {
	url := scriptProbeURL
	if url == "" {
		url = config.InstallURL(cfg)
	}
	if url == "" {
		return recordedOnly(cs, recordedVer, "github unresolved"), nil
	}
	sc := &channel.Script{InstallURL: url}
	rs, err := sc.Probe(ctx)
	if err != nil {
		return recordedOnly(cs, recordedVer, "not published yet"), probeFailedWarning("install.sh", err)
	}
	warn := reconcileInto(&cs, "script", recordedVer, rs)
	return cs, warn
}

// assessGoinstall は module proxy で現タグの go install 可否を確認する(記録は持たない)。
func assessGoinstall(ctx context.Context, cs statusChannel, module, tag string) (statusChannel, *output.Warning) {
	if tag == "" {
		cs.Source = state.SourceRecorded
		cs.Published = false
		cs.Reason = "no tag; `go install` resolves no version"
		return cs, nil
	}
	gi := &channel.GoInstall{Module: module, Version: tag, Proxy: goinstallProxy}
	rs, err := gi.Probe(ctx)
	if err != nil {
		cs.Source = state.SourceRecorded
		cs.Reason = "proxy unreachable"
		return cs, probeFailedWarning("module proxy", err)
	}
	// goinstall は発行物を持たない(記録 vs 実体の drift 概念が無い)。proxy 到達性を見せる。
	cs.Source = state.SourceProbed
	cs.Published = rs.Found
	if rs.Found {
		cs.Version = tag
	} else {
		cs.Reason = "not resolvable via `go install` yet (push the tag; public repo)"
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
