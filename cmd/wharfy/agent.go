package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

// runAgent は ①「聞けば分かる」の一枚出力。registry から生成するので実体とズレない(05)。
// --json は schemas/agent.json に valid な AgentDoc を出す。
func runAgent(asJSON bool) error {
	doc := registry.BuildAgentDoc(versionLine())
	if asJSON {
		s, err := output.Marshal(doc)
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stdout, s)
		return nil
	}
	printAgentHuman(doc)
	return nil
}

// printAgentHuman は人間向けの体裁(samples/cmd_agent.go の runAgent 準拠)。
func printAgentHuman(doc registry.AgentDoc) {
	fmt.Println("wharfy — ship one binary to every channel. Read this once, then drive.")
	fmt.Printf("version: %s\n", doc.Version)
	fmt.Println("\nCOMMANDS (usual order)")
	for _, c := range doc.Commands {
		name := c.Name
		if c.Args != "" {
			name += " " + c.Args
		}
		line := fmt.Sprintf("  wharfy %-18s %s", name, c.Summary)
		if len(c.Next) > 0 {
			line += "   → next: " + strings.Join(c.Next, " | ")
		}
		fmt.Println(line)
	}
	if len(doc.Channels) > 0 {
		names := make([]string, 0, len(doc.Channels))
		for _, ch := range doc.Channels {
			names = append(names, fmt.Sprintf("%s(%s)", ch.Name, ch.Kind))
		}
		fmt.Printf("\nCHANNELS  %s\n", strings.Join(names, " "))
	}
	fmt.Println("\nEvery command takes --json and ends with a next: block.")
	fmt.Printf("START HERE\n  %s\n", doc.Start)
}
