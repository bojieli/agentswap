package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Definitions are explicit configuration transfers, separate from session
// history: resuming a conversation must never overwrite an installed agent.
type AgentDefinition struct {
	Name            string                   `json:"name"`
	Description     string                   `json:"description"`
	Prompt          string                   `json:"prompt"`
	Model           string                   `json:"model,omitempty"`
	Effort          string                   `json:"effort,omitempty"`
	Tools           []string                 `json:"tools,omitempty"`
	ToolsRestricted bool                     `json:"tools_restricted,omitempty"`
	DeniedTools     []string                 `json:"denied_tools,omitempty"`
	Native          map[Agent]map[string]any `json:"native,omitempty"`
}

type DefinitionOptions struct {
	CWD       string
	SourceDir string
	TargetDir string
	AgentFile string // Kimi's root agent YAML, or a legacy Codex role config.
	Model     string
	DryRun    bool
	Strict    bool
}

type DefinitionResult struct {
	Definitions []AgentDefinition
	Files       []string
	Warnings    []string
	Launch      []string
}

type definitionManifest struct {
	Version     int               `json:"version"`
	Definitions []AgentDefinition `json:"definitions"`
}

func definitionDir(agent Agent, cwd string) string {
	return filepath.Join(cwd, "."+string(agent), "agents")
}

func safeDefinitionName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func stringList(v any) []string {
	out := []string{}
	switch v := v.(type) {
	case string:
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	case []any:
		for _, s := range v {
			if value, ok := s.(string); ok {
				out = append(out, value)
			}
		}
	}
	return out
}

func readDefinitionFile(agent Agent, path string) (map[string]any, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	if len(b) > 8<<20 {
		return nil, "", fmt.Errorf("agent definition %s exceeds 8 MiB", path)
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	switch filepath.Ext(path) {
	case ".md":
		if !strings.HasPrefix(text, "---\n") {
			return nil, "", fmt.Errorf("agent definition %s has no YAML frontmatter", path)
		}
		end := strings.Index(text[4:], "\n---")
		if end < 0 {
			return nil, "", fmt.Errorf("unterminated frontmatter in %s", path)
		}
		end += 4
		config, err := parseDefinitionYAML(text[4:end])
		return config, strings.TrimPrefix(text[end+4:], "\n"), err
	case ".toml":
		config, err := parseDefinitionTOML(text)
		return config, stringValue(config["developer_instructions"]), err
	case ".yaml", ".yml":
		config, err := parseDefinitionYAML(text)
		return config, "", err
	case ".json":
		var config map[string]any
		err := json.Unmarshal(b, &config)
		return config, "", err
	default:
		return nil, "", fmt.Errorf("unsupported %s agent definition file %s", agent, path)
	}
}

func definitionFromConfig(agent Agent, name, prompt string, cfg map[string]any) AgentDefinition {
	if value := stringValue(cfg["name"]); value != "" {
		name = value
	}
	d := AgentDefinition{Name: name, Description: stringValue(cfg["description"]), Prompt: prompt, Model: stringValue(cfg["model"]), Effort: stringValue(cfg["effort"]), Native: map[Agent]map[string]any{agent: cfg}}
	if agent == Codex {
		d.Effort = stringValue(cfg["model_reasoning_effort"])
	}
	if d.Description == "" {
		d.Description = "Imported " + name + " agent"
	}
	d.Tools = stringList(cfg["tools"])
	_, d.ToolsRestricted = cfg["tools"]
	if agent == Codex || agent == OpenCode {
		d.ToolsRestricted = false
	}
	d.DeniedTools = stringList(cfg["disallowedTools"])
	if agent == OpenCode {
		if tools, ok := cfg["tools"].(map[string]any); ok {
			for name, value := range tools {
				if enabled, ok := value.(bool); ok {
					if enabled {
						d.Tools = append(d.Tools, name)
					} else {
						d.DeniedTools = append(d.DeniedTools, name)
					}
				}
			}
		}
		if permissions, ok := cfg["permission"].(map[string]any); ok {
			for name, value := range permissions {
				if stringValue(value) == "deny" {
					d.DeniedTools = append(d.DeniedTools, name)
				}
			}
		}
	}
	if agent == Codex && stringValue(cfg["sandbox_mode"]) == "read-only" {
		d.DeniedTools = append(d.DeniedTools, "edit", "write", "shell")
	}
	if agent == Kimi {
		d.DeniedTools = stringList(cfg["exclude_tools"])
	}
	sort.Strings(d.DeniedTools)
	return d
}

func DiscoverDefinitions(agent Agent, opts DefinitionOptions) ([]AgentDefinition, []string, error) {
	byName := map[string]AgentDefinition{}
	savedDefinitions := map[string]AgentDefinition{}
	var warnings []string
	loadDir := func(dir string) error {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		var archived definitionManifest
		if b, err := os.ReadFile(filepath.Join(dir, "agentswap-definitions.json")); err == nil {
			if err := json.Unmarshal(b, &archived); err != nil {
				return err
			}
			if archived.Version != 1 {
				return fmt.Errorf("unsupported definition manifest version")
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		for _, saved := range archived.Definitions {
			savedDefinitions[saved.Name] = saved
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			ext := filepath.Ext(entry.Name())
			if agent == Claude || agent == OpenCode {
				if ext != ".md" {
					continue
				}
			} else if agent == Codex {
				if ext != ".toml" {
					continue
				}
			} else {
				if ext != ".yaml" && ext != ".yml" {
					continue
				}
				if entry.Name() == "agent.yaml" {
					continue
				}
			}
			path := filepath.Join(dir, entry.Name())
			cfg, prompt, err := readDefinitionFile(agent, path)
			if err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
			if agent == Kimi {
				cfg, prompt, err = resolveKimiDefinition(path, map[string]bool{}, 0)
				if err != nil {
					return err
				}
			}
			if agent == OpenCode && stringValue(cfg["mode"]) == "primary" {
				continue
			}
			d := definitionFromConfig(agent, strings.TrimSuffix(entry.Name(), ext), prompt, cfg)
			for _, saved := range archived.Definitions {
				if saved.Name == d.Name {
					if d.Native == nil {
						d.Native = map[Agent]map[string]any{}
					}
					for a, native := range saved.Native {
						if a != agent {
							d.Native[a] = native
						}
					}
				}
			}
			byName[d.Name] = d
		}
		return nil
	}
	if opts.SourceDir != "" {
		if _, err := os.Stat(opts.SourceDir); err != nil {
			return nil, nil, err
		}
		if err := loadDir(opts.SourceDir); err != nil {
			return nil, nil, err
		}
	} else {
		var global string
		switch agent {
		case Claude:
			global = filepath.Join(claudeRoot(), "agents")
		case Codex:
			global = filepath.Join(codexRoot(), "agents")
		case OpenCode:
			global = filepath.Join(envDir("XDG_CONFIG_HOME", filepath.Join(homeDir(), ".config")), "opencode", "agents")
		case Kimi:
			global = filepath.Join(kimiCodeRoot(), "agents")
		}
		if err := loadDir(global); err != nil {
			return nil, nil, err
		}
		if err := loadDir(definitionDir(agent, opts.CWD)); err != nil {
			return nil, nil, err
		}
	}
	if agent == Kimi {
		root := opts.AgentFile
		if root == "" {
			dir := opts.SourceDir
			if dir == "" {
				dir = definitionDir(Kimi, opts.CWD)
			}
			root = filepath.Join(dir, "agent.yaml")
		}
		if _, err := os.Stat(root); err == nil {
			cfg, _, err := resolveKimiDefinition(root, map[string]bool{}, 0)
			if err != nil {
				return nil, nil, err
			}
			if subs, ok := cfg["subagents"].(map[string]any); ok {
				for name, raw := range subs {
					sub, ok := raw.(map[string]any)
					if !ok {
						return nil, nil, fmt.Errorf("invalid Kimi subagent %s", name)
					}
					path := stringValue(sub["path"])
					if path == "" {
						return nil, nil, fmt.Errorf("Kimi subagent %s has no path", name)
					}
					config, prompt, err := resolveKimiDefinition(filepath.Join(filepath.Dir(root), path), map[string]bool{}, 0)
					if err != nil {
						return nil, nil, err
					}
					config["name"] = name
					config["description"] = stringValue(sub["description"])
					byName[name] = definitionFromConfig(Kimi, name, prompt, config)
				}
			}
		} else if opts.AgentFile != "" || !os.IsNotExist(err) {
			return nil, nil, err
		}
	}
	if agent == Codex && opts.AgentFile != "" {
		cfg, _, err := readDefinitionFile(Codex, opts.AgentFile)
		if err != nil {
			return nil, nil, err
		}
		roles, _ := cfg["agents"].(map[string]any)
		for name, raw := range roles {
			role, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			path := stringValue(role["config_file"])
			if path == "" {
				continue
			}
			config, prompt, err := readDefinitionFile(Codex, filepath.Join(filepath.Dir(opts.AgentFile), path))
			if err != nil {
				return nil, nil, err
			}
			config["name"] = name
			config["description"] = role["description"]
			byName[name] = definitionFromConfig(Codex, name, prompt, config)
		}
	}
	if agent == OpenCode && opts.AgentFile != "" {
		cfg, _, err := readDefinitionFile(OpenCode, opts.AgentFile)
		if err != nil {
			return nil, nil, err
		}
		agents, _ := cfg["agent"].(map[string]any)
		for name, raw := range agents {
			config, ok := raw.(map[string]any)
			if !ok || stringValue(config["mode"]) == "primary" {
				continue
			}
			prompt := stringValue(config["prompt"])
			if strings.HasPrefix(prompt, "{file:") && strings.HasSuffix(prompt, "}") {
				path := prompt[6 : len(prompt)-1]
				b, err := os.ReadFile(filepath.Join(filepath.Dir(opts.AgentFile), path))
				if err != nil {
					return nil, nil, err
				}
				prompt = string(b)
			}
			byName[name] = definitionFromConfig(OpenCode, name, prompt, config)
		}
	}
	var definitions []AgentDefinition
	for _, d := range byName {
		if saved, ok := savedDefinitions[d.Name]; ok {
			for a, cfg := range saved.Native {
				if a != agent {
					d.Native[a] = cfg
				}
			}
			if d.Model == "" {
				d.Model = saved.Model
			}
			if d.Effort == "" {
				d.Effort = saved.Effort
			}
		}
		if !safeDefinitionName(d.Name) {
			return nil, nil, fmt.Errorf("agent name %q cannot be represented as a safe native filename", d.Name)
		}
		definitions = append(definitions, d)
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	return definitions, warnings, nil
}

func resolveKimiDefinition(path string, seen map[string]bool, depth int) (map[string]any, string, error) {
	canonical, err := canonicalPath(path)
	if err != nil {
		return nil, "", err
	}
	if seen[canonical] || depth >= 32 {
		return nil, "", fmt.Errorf("cyclic or excessive Kimi agent inheritance at %s", path)
	}
	seen[canonical] = true
	defer delete(seen, canonical)
	root, _, err := readDefinitionFile(Kimi, path)
	if err != nil {
		return nil, "", err
	}
	cfg, ok := root["agent"].(map[string]any)
	if !ok {
		return nil, "", fmt.Errorf("Kimi agent %s has no agent mapping", path)
	}
	merged := map[string]any{}
	prompt := ""
	if extend := stringValue(cfg["extend"]); extend != "" && extend != "default" && extend != "okabe" {
		base, basePrompt, err := resolveKimiDefinition(filepath.Join(filepath.Dir(path), extend), seen, depth+1)
		if err != nil {
			return nil, "", err
		}
		for key, value := range base {
			merged[key] = value
		}
		prompt = basePrompt
	}
	for key, value := range cfg {
		if key == "system_prompt_args" {
			args := map[string]any{}
			if base, ok := merged[key].(map[string]any); ok {
				for k, v := range base {
					args[k] = v
				}
			}
			if own, ok := value.(map[string]any); ok {
				for k, v := range own {
					args[k] = v
				}
			}
			value = args
		}
		merged[key] = value
	}
	if value := stringValue(cfg["system_prompt_path"]); value != "" {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(path), value))
		if err != nil {
			return nil, "", err
		}
		prompt = string(b)
	}
	if args, ok := merged["system_prompt_args"].(map[string]any); ok {
		if prompt == "" {
			prompt = stringValue(args["ROLE_ADDITIONAL"])
		}
		for name, value := range args {
			prompt = strings.ReplaceAll(prompt, "${"+name+"}", fmt.Sprint(value))
		}
	}
	return merged, prompt, nil
}

// Logical tool capabilities are mapped only when there is a known equivalent.
var definitionTools = map[string]map[Agent][]string{
	"read":  {Claude: {"Read"}, OpenCode: {"read"}, Kimi: {"kimi_cli.tools.file:ReadFile"}},
	"glob":  {Claude: {"Glob"}, OpenCode: {"glob"}, Kimi: {"kimi_cli.tools.file:Glob"}},
	"grep":  {Claude: {"Grep"}, OpenCode: {"grep"}, Kimi: {"kimi_cli.tools.file:Grep"}},
	"shell": {Claude: {"Bash"}, OpenCode: {"bash"}, Kimi: {"kimi_cli.tools.shell:Shell"}},
	"write": {Claude: {"Write"}, OpenCode: {"write"}, Kimi: {"kimi_cli.tools.file:WriteFile"}},
	"edit":  {Claude: {"Edit"}, OpenCode: {"edit"}, Kimi: {"kimi_cli.tools.file:StrReplaceFile"}},
	"web":   {Claude: {"WebSearch"}, OpenCode: {"websearch"}, Kimi: {"kimi_cli.tools.web:SearchWeb"}},
	"fetch": {Claude: {"WebFetch"}, OpenCode: {"webfetch"}, Kimi: {"kimi_cli.tools.web:FetchURL"}},
	"agent": {Claude: {"Agent"}, OpenCode: {"task"}, Kimi: {"kimi_cli.tools.agent:Agent"}},
}

func mapDefinitionTools(names []string, target Agent) ([]string, []string) {
	out := []string{}
	var warnings []string
	for _, name := range names {
		found := false
		for capability, mapping := range definitionTools {
			if name == capability && len(mapping[target]) > 0 {
				out = append(out, mapping[target]...)
				found = true
			}
			for _, aliases := range mapping {
				for _, alias := range aliases {
					if strings.EqualFold(alias, name) || name == "Task" && alias == "Agent" {
						if tools := mapping[target]; len(tools) > 0 {
							out = append(out, tools...)
							found = true
						}
					}
				}
			}
		}
		if !found {
			warnings = append(warnings, fmt.Sprintf("tool %q has no enforceable equivalent in %s; its source policy is retained in the definition manifest", name, target.Display()))
		}
	}
	sort.Strings(out)
	unique := out[:0]
	for _, name := range out {
		if len(unique) == 0 || unique[len(unique)-1] != name {
			unique = append(unique, name)
		}
	}
	return unique, warnings
}

func TransferDefinitions(source, target Agent, opts DefinitionOptions) (DefinitionResult, error) {
	if source == target {
		return DefinitionResult{}, fmt.Errorf("source and target agents are the same")
	}
	defs, warnings, err := DiscoverDefinitions(source, opts)
	if err != nil {
		return DefinitionResult{}, err
	}
	if len(defs) == 0 {
		return DefinitionResult{}, fmt.Errorf("no custom %s agents found", source.Display())
	}
	dir := opts.TargetDir
	if dir == "" {
		dir = definitionDir(target, opts.CWD)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return DefinitionResult{}, err
	}
	result := DefinitionResult{Definitions: defs, Warnings: warnings}
	contents := map[string][]byte{}
	rootSubs := map[string]any{}
	for _, d := range defs {
		cfg := map[string]any{}
		for k, v := range d.Native[target] {
			cfg[k] = v
		}
		cfg["description"] = d.Description
		model := opts.Model
		if model == "" && d.Native[target] != nil {
			model = stringValue(d.Native[target]["model"])
		}
		if model == "" && d.Model != "" {
			result.Warnings = appendUnique(result.Warnings, fmt.Sprintf("agent %s: model %q is provider-specific; %s will inherit its configured model (use --model to override)", d.Name, d.Model, target.Display()))
		}
		tools, toolWarnings := mapDefinitionTools(d.Tools, target)
		deny, denyWarnings := mapDefinitionTools(d.DeniedTools, target)
		if d.Native[target] == nil {
			for _, w := range append(toolWarnings, denyWarnings...) {
				result.Warnings = appendUnique(result.Warnings, "agent "+d.Name+": "+w)
			}
		}
		if source == Codex && target != Codex && d.Native[target] == nil && stringValue(d.Native[source]["sandbox_mode"]) == "read-only" {
			result.Warnings = appendUnique(result.Warnings, "agent "+d.Name+": the Codex read-only sandbox cannot be reproduced exactly; shell, edit, and write tools are disabled in the destination")
		}
		// Unknown settings remain in the manifest and are never activated as
		// hooks, credentials or permission overrides in another harness.
		known := map[string]bool{"name": true, "description": true, "model": true, "effort": true, "model_reasoning_effort": true, "tools": true, "disallowedTools": true, "exclude_tools": true, "mode": true, "developer_instructions": true, "system_prompt_path": true, "system_prompt_args": true, "extend": true, "subagents": true, "prompt": true, "sandbox_mode": true}
		if d.Native[target] == nil {
			for k := range d.Native[source] {
				if !known[k] {
					result.Warnings = appendUnique(result.Warnings, fmt.Sprintf("agent %s: %s setting %q is retained in the manifest; it is not activated in %s", d.Name, source, k, target))
				}
			}
		}
		switch target {
		case Claude:
			cfg["name"] = d.Name
			if model != "" {
				cfg["model"] = model
			}
			if d.Effort != "" {
				cfg["effort"] = d.Effort
			}
			if d.Native[target] == nil {
				if d.ToolsRestricted || len(d.Tools) > 0 {
					cfg["tools"] = tools
				}
				if len(deny) > 0 {
					cfg["disallowedTools"] = deny
				}
			}
			b, err := json.MarshalIndent(cfg, "", "  ")
			if err != nil {
				return result, err
			}
			contents[d.Name+".md"] = []byte("---\n" + string(b) + "\n---\n" + d.Prompt)
		case Codex:
			cfg["name"] = d.Name
			cfg["developer_instructions"] = d.Prompt
			if model != "" {
				cfg["model"] = model
			}
			if d.Effort != "" {
				cfg["model_reasoning_effort"] = d.Effort
			}
			if d.Native[target] == nil && (d.ToolsRestricted || len(d.Tools) > 0 || len(d.DeniedTools) > 0) {
				cfg["sandbox_mode"] = "read-only"
				result.Warnings = appendUnique(result.Warnings, "agent "+d.Name+": Codex has no equivalent tool allowlist; a read-only sandbox is used and the precise policy is retained in the manifest")
			}
			b, err := encodeDefinitionTOML(cfg)
			if err != nil {
				return result, err
			}
			contents[d.Name+".toml"] = b
		case OpenCode:
			cfg["mode"] = "subagent"
			delete(cfg, "name")
			if model != "" {
				cfg["model"] = model
			}
			if d.Native[target] == nil {
				policy := map[string]any{}
				if d.ToolsRestricted || len(d.Tools) > 0 {
					policy["*"] = "deny"
					for _, tool := range tools {
						policy[tool] = "allow"
					}
				}
				for _, tool := range deny {
					policy[tool] = "deny"
				}
				if len(policy) > 0 {
					cfg["permission"] = policy
				}
			}
			b, err := json.MarshalIndent(cfg, "", "  ")
			if err != nil {
				return result, err
			}
			contents[d.Name+".md"] = []byte("---\n" + string(b) + "\n---\n" + d.Prompt)
		case Kimi:
			delete(cfg, "description")
			delete(cfg, "model")
			cfg["name"] = d.Name
			cfg["extend"] = "default"
			cfg["system_prompt_path"] = "./" + d.Name + ".prompt.md"
			delete(cfg, "subagents")
			delete(cfg, "system_prompt_args")
			if d.Native[target] == nil {
				if d.ToolsRestricted || len(d.Tools) > 0 {
					cfg["tools"] = tools
				}
				if len(deny) > 0 {
					cfg["exclude_tools"] = deny
				}
			}
			if model != "" {
				result.Warnings = appendUnique(result.Warnings, "agent "+d.Name+": Kimi agent YAML does not select a model; use the destination CLI's --model flag")
			}
			if d.Effort != "" {
				result.Warnings = appendUnique(result.Warnings, "agent "+d.Name+": Kimi agent YAML has no reasoning effort field")
			}
			b, err := json.MarshalIndent(map[string]any{"version": 1, "agent": cfg}, "", "  ")
			if err != nil {
				return result, err
			}
			contents[d.Name+".yaml"] = append(b, '\n')
			contents[d.Name+".prompt.md"] = []byte(d.Prompt)
			rootSubs[d.Name] = map[string]any{"path": "./" + d.Name + ".yaml", "description": d.Description}
		default:
			return result, fmt.Errorf("unsupported target %q", target)
		}
	}
	if target == Kimi {
		b, err := json.MarshalIndent(map[string]any{"version": 1, "agent": map[string]any{"extend": "default", "subagents": rootSubs}}, "", "  ")
		if err != nil {
			return result, err
		}
		contents["agent.yaml"] = append(b, '\n')
		result.Launch = []string{"kimi", "--agent-file", filepath.Join(dir, "agent.yaml")}
		if opts.Model != "" {
			result.Launch = append(result.Launch, "--model", opts.Model)
		}
	}
	manifest, err := json.MarshalIndent(definitionManifest{1, defs}, "", "  ")
	if err != nil {
		return result, err
	}
	contents["agentswap-definitions.json"] = append(manifest, '\n')
	for name := range contents {
		path := filepath.Join(dir, name)
		if _, err := os.Lstat(path); err == nil {
			return result, fmt.Errorf("refusing to overwrite existing agent file %s", path)
		} else if !os.IsNotExist(err) {
			return result, err
		}
		result.Files = append(result.Files, path)
	}
	sort.Strings(result.Files)
	sort.Strings(result.Warnings)
	if opts.Strict && len(result.Warnings) > 0 {
		return result, fmt.Errorf("strict definition transfer refused: %s", strings.Join(result.Warnings, "; "))
	}
	if opts.DryRun {
		return result, nil
	}
	if err := ensureDir(dir); err != nil {
		return result, err
	}
	var written []string
	for _, path := range result.Files {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			written = append(written, path)
			_, err = f.Write(contents[filepath.Base(path)])
			if syncErr := f.Sync(); err == nil {
				err = syncErr
			}
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
		}
		if err != nil {
			for _, p := range written {
				removeIfExists(p)
			}
			return result, err
		}
	}
	return result, nil
}

func encodeDefinitionTOML(cfg map[string]any) ([]byte, error) {
	var out strings.Builder
	var encode func(any) (string, error)
	encode = func(value any) (string, error) {
		switch value := value.(type) {
		case map[string]any:
			var parts []string
			for key, item := range value {
				text, err := encode(item)
				if err != nil {
					return "", err
				}
				parts = append(parts, strconv.Quote(key)+" = "+text)
			}
			sort.Strings(parts)
			return "{" + strings.Join(parts, ", ") + "}", nil
		case []any:
			var parts []string
			for _, item := range value {
				text, err := encode(item)
				if err != nil {
					return "", err
				}
				parts = append(parts, text)
			}
			return "[" + strings.Join(parts, ", ") + "]", nil
		case nil:
			return "", fmt.Errorf("TOML has no null value")
		default:
			b, err := json.Marshal(value)
			return string(b), err
		}
	}
	var write func(map[string]any, []string) error
	write = func(m map[string]any, prefix []string) error {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, ok := m[key].(map[string]any); ok {
				continue
			}
			value := m[key]
			if value == nil {
				continue
			}
			b, err := encode(value)
			if err != nil {
				return err
			}
			fmt.Fprintf(&out, "%s = %s\n", strconv.Quote(key), b)
		}
		for _, key := range keys {
			if sub, ok := m[key].(map[string]any); ok {
				path := append(append([]string(nil), prefix...), strconv.Quote(key))
				fmt.Fprintf(&out, "\n[%s]\n", strings.Join(path, "."))
				if err := write(sub, path); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := write(cfg, nil); err != nil {
		return nil, err
	}
	return []byte(out.String()), nil
}
