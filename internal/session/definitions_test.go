package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func definitionFixture(t *testing.T, agent Agent) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{}
	switch agent {
	case Claude:
		files["reviewer.md"] = "---\nname: reviewer\ndescription: Review code\ntools: [Read, Grep]\ndisallowedTools: [Edit, Write]\nmodel: sonnet\neffort: high\n---\nReview carefully. Preserve `quotes` and Unicode: 日本語.\n"
	case Codex:
		files["reviewer.toml"] = "name = \"reviewer\"\ndescription = \"Review code\"\nmodel = \"gpt-5.4\"\nmodel_reasoning_effort = \"high\"\nsandbox_mode = \"read-only\"\ndeveloper_instructions = '''\nReview carefully. Preserve `quotes` and Unicode: 日本語.\n'''\n"
	case OpenCode:
		files["reviewer.md"] = "---\ndescription: Review code\nmode: subagent\nmodel: anthropic/claude-sonnet-4-6\npermission:\n  edit: deny\n  write: deny\n---\nReview carefully. Preserve `quotes` and Unicode: 日本語.\n"
	case Kimi:
		files["reviewer.yaml"] = "version: 1\nagent:\n  extend: default\n  name: reviewer\n  system_prompt_path: ./reviewer.prompt.md\n  tools:\n    - kimi_cli.tools.file:ReadFile\n    - kimi_cli.tools.file:Grep\n  exclude_tools:\n    - kimi_cli.tools.file:WriteFile\n    - kimi_cli.tools.file:StrReplaceFile\n"
		files["reviewer.prompt.md"] = "Review carefully. Preserve `quotes` and Unicode: 日本語.\n"
		files["agent.yaml"] = "version: 1\nagent:\n  extend: default\n  subagents:\n    reviewer:\n      path: ./reviewer.yaml\n      description: Review code\n"
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDefinitionTransfersAllDirections(t *testing.T) {
	for _, source := range Agents() {
		for _, target := range Agents() {
			if source == target {
				continue
			}
			t.Run(string(source)+"-"+string(target), func(t *testing.T) {
				isolatedHomes(t)
				from := definitionFixture(t, source)
				to := filepath.Join(t.TempDir(), "agents")
				result, err := TransferDefinitions(source, target, DefinitionOptions{CWD: t.TempDir(), SourceDir: from, TargetDir: to})
				if err != nil {
					t.Fatal(err)
				}
				if len(result.Definitions) != 1 || len(result.Files) < 2 {
					t.Fatalf("result = %#v", result)
				}
				defs, _, err := DiscoverDefinitions(target, DefinitionOptions{SourceDir: to})
				if err != nil {
					t.Fatal(err)
				}
				if len(defs) != 1 {
					t.Fatalf("definitions = %#v", defs)
				}
				if defs[0].Name != "reviewer" || !strings.Contains(defs[0].Prompt, "日本語") || defs[0].Description != "Review code" {
					t.Fatalf("lost definition: %#v", defs[0])
				}
				// Round-trip policies that cannot be expressed in the intermediate
				// harness are restored from the inert provenance manifest.
				back := filepath.Join(t.TempDir(), "agents")
				_, err = TransferDefinitions(target, source, DefinitionOptions{SourceDir: to, TargetDir: back})
				if err != nil {
					t.Fatal(err)
				}
				defs, _, err = DiscoverDefinitions(source, DefinitionOptions{SourceDir: back})
				if err != nil {
					t.Fatal(err)
				}
				if len(defs) != 1 || !strings.Contains(defs[0].Prompt, "日本語") {
					t.Fatalf("round trip = %#v", defs)
				}
			})
		}
	}
}

func TestDefinitionDryRunStrictAndCollisions(t *testing.T) {
	from := definitionFixture(t, Claude)
	to := filepath.Join(t.TempDir(), "uncreated")
	opts := DefinitionOptions{SourceDir: from, TargetDir: to, DryRun: true}
	r, err := TransferDefinitions(Claude, Codex, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) == 0 {
		t.Fatal("tool-policy loss was not reported")
	}
	if _, err := os.Stat(to); !os.IsNotExist(err) {
		t.Fatal("dry run wrote files")
	}
	opts.DryRun = false
	opts.Strict = true
	if _, err := TransferDefinitions(Claude, Codex, opts); err == nil {
		t.Fatal("strict transfer accepted policy loss")
	}
	if _, err := os.Stat(to); !os.IsNotExist(err) {
		t.Fatal("strict refusal wrote files")
	}
	opts.Strict = false
	if _, err := TransferDefinitions(Claude, Codex, opts); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(to, "reviewer.toml")
	before, _ := os.ReadFile(path)
	if _, err := TransferDefinitions(Claude, Codex, opts); err == nil {
		t.Fatal("existing files overwritten")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("existing agent changed")
	}
}

func TestDefinitionInheritanceAndLegacyRoles(t *testing.T) {
	t.Run("Kimi inheritance", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("Role: ${ROLE}."), 0o600)
		os.WriteFile(filepath.Join(dir, "base.yaml"), []byte("version: 1\nagent:\n  extend: default\n  system_prompt_path: ./prompt.md\n  tools: [kimi_cli.tools.file:ReadFile]\n"), 0o600)
		os.WriteFile(filepath.Join(dir, "reviewer.yaml"), []byte("version: 1\nagent:\n  extend: ./base.yaml\n  name: reviewer\n  system_prompt_args:\n    ROLE: reviewer\n"), 0o600)
		cfg, prompt, err := resolveKimiDefinition(filepath.Join(dir, "reviewer.yaml"), map[string]bool{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if prompt != "Role: reviewer." || len(stringList(cfg["tools"])) != 1 {
			t.Fatalf("inheritance = %#v %q", cfg, prompt)
		}
		os.WriteFile(filepath.Join(dir, "base.yaml"), []byte("version: 1\nagent:\n  extend: ./reviewer.yaml\n"), 0o600)
		if _, _, err := resolveKimiDefinition(filepath.Join(dir, "reviewer.yaml"), map[string]bool{}, 0); err == nil {
			t.Fatal("inheritance cycle accepted")
		}
	})
	t.Run("Codex config_file", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "role.toml"), []byte("developer_instructions = \"Review carefully\"\nsandbox_mode = \"read-only\"\n"), 0o600)
		config := filepath.Join(dir, "config.toml")
		os.WriteFile(config, []byte("[agents.reviewer]\ndescription = \"Review code\"\nconfig_file = \"role.toml\"\n"), 0o600)
		defs, _, err := DiscoverDefinitions(Codex, DefinitionOptions{SourceDir: t.TempDir(), AgentFile: config})
		if err != nil {
			t.Fatal(err)
		}
		if len(defs) != 1 || defs[0].Name != "reviewer" || defs[0].Prompt != "Review carefully" {
			t.Fatalf("roles = %#v", defs)
		}
	})
	t.Run("OpenCode JSON config", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("Review carefully"), 0o600)
		config := filepath.Join(dir, "opencode.json")
		os.WriteFile(config, []byte(`{"agent":{"reviewer":{"mode":"subagent","description":"Review code","prompt":"{file:./prompt.md}","permission":{"edit":"deny"}}}}`), 0o600)
		defs, _, err := DiscoverDefinitions(OpenCode, DefinitionOptions{SourceDir: t.TempDir(), AgentFile: config})
		if err != nil {
			t.Fatal(err)
		}
		if len(defs) != 1 || defs[0].Prompt != "Review carefully" {
			t.Fatalf("agents = %#v", defs)
		}
	})
}

func TestDefinitionConfigParsers(t *testing.T) {
	for _, text := range []string{"name: one\nname: two", "name: *alias", "name: !custom value", "tools: [Read", "name:\n\tvalue: bad"} {
		if _, err := parseDefinitionYAML(text); err == nil {
			t.Fatalf("accepted unsupported or invalid YAML %q", text)
		}
	}
	for _, text := range []string{"[[skills.config]]\npath = 'x'", "name = 'one'\nname = 'two'", "prompt = '''unterminated"} {
		if _, err := parseDefinitionTOML(text); err == nil {
			t.Fatalf("accepted unsupported or invalid TOML %q", text)
		}
	}
	cfg, err := parseDefinitionYAML("description: |\n  Review # literal comment\n  the code.\ntools: [Read, Grep]\npermission: {edit: deny, bash: ask}\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stringValue(cfg["description"]), "# literal comment") {
		t.Fatalf("lost block content %#v", cfg)
	}
	if safeDefinitionName("../outside") || safeDefinitionName("a/b") || safeDefinitionName("..") {
		t.Fatal("path traversal name accepted")
	}
}

func TestEmptyDefinitionAllowlistStaysEmpty(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "text-only.md"), []byte("---\nname: text-only\ndescription: Answer without tools\ntools: []\n---\nOnly discuss.\n"), 0o600)
	for _, target := range []Agent{Kimi, OpenCode, Codex} {
		t.Run(string(target), func(t *testing.T) {
			out := t.TempDir()
			result, err := TransferDefinitions(Claude, target, DefinitionOptions{SourceDir: dir, TargetDir: out})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Definitions[0].ToolsRestricted {
				t.Fatal("empty allowlist became inherited tools")
			}
			if target == Kimi {
				cfg, _, err := resolveKimiDefinition(filepath.Join(out, "text-only.yaml"), map[string]bool{}, 0)
				if err != nil {
					t.Fatal(err)
				}
				tools, exists := cfg["tools"]
				if !exists || tools == nil || len(stringList(tools)) != 0 {
					t.Fatalf("tools = %#v", tools)
				}
			}
		})
	}
}
