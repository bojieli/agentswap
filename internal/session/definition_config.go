package session

// Agent definition files use a small data-oriented subset of YAML/TOML.
// This parser intentionally rejects features it cannot represent (YAML aliases,
// tags, TOML arrays of tables), rather than silently changing an agent policy.
// Session transfer and the credential-holding proxy remain dependency-free.
import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func configValue(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.HasPrefix(raw, "[") || strings.HasPrefix(raw, "{") {
		var value any
		if json.Unmarshal([]byte(raw), &value) == nil {
			return value, nil
		}
		if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
			out := []any{}
			for _, item := range splitConfig(raw[1:len(raw)-1], ',') {
				if strings.TrimSpace(item) == "" {
					continue
				}
				value, err := configValue(item)
				if err != nil {
					return nil, err
				}
				out = append(out, value)
			}
			return out, nil
		}
		if strings.HasPrefix(raw, "{") && strings.HasSuffix(raw, "}") {
			out := map[string]any{}
			for _, item := range splitConfig(raw[1:len(raw)-1], ',') {
				parts := splitConfig(item, ':')
				if len(parts) != 2 {
					parts = splitConfig(item, '=')
				}
				if len(parts) != 2 {
					return nil, fmt.Errorf("unsupported inline mapping %q", raw)
				}
				key, err := configKey(parts[0])
				if err != nil {
					return nil, err
				}
				value, err := configValue(parts[1])
				if err != nil {
					return nil, err
				}
				out[key] = value
			}
			return out, nil
		}
		return nil, fmt.Errorf("unterminated configuration value %q", raw)
	}
	if strings.HasPrefix(raw, "\"") {
		v, err := strconv.Unquote(raw)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	if strings.HasPrefix(raw, "'") {
		if !strings.HasSuffix(raw, "'") || len(raw) < 2 {
			return nil, fmt.Errorf("unterminated literal")
		}
		return strings.ReplaceAll(raw[1:len(raw)-1], "''", "'"), nil
	}
	switch raw {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null", "~":
		return nil, nil
	}
	if n, err := strconv.ParseFloat(raw, 64); err == nil {
		return n, nil
	}
	if strings.ContainsAny(raw[:1], "&*!|>") {
		return nil, fmt.Errorf("unsupported YAML feature %q", raw)
	}
	return raw, nil
}

func configKey(raw string) (string, error) {
	v, err := configValue(raw)
	if err != nil {
		return "", err
	}
	key, ok := v.(string)
	if !ok || key == "" {
		return "", fmt.Errorf("invalid configuration key %q", raw)
	}
	return key, nil
}

func splitConfig(raw string, delimiter byte) []string {
	var out []string
	start, depth := 0, 0
	var quote byte
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if c == '[' || c == '{' {
			depth++
		}
		if c == ']' || c == '}' {
			depth--
		}
		if c == delimiter && depth == 0 {
			out = append(out, raw[start:i])
			start = i + 1
		}
	}
	return append(out, raw[start:])
}

func configComment(raw string) string {
	parts := splitConfig(raw, '#')
	return strings.TrimSpace(parts[0])
}

func parseDefinitionTOML(text string) (map[string]any, error) {
	root := map[string]any{}
	current := root
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := configComment(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if strings.HasPrefix(line, "[[") || !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("unsupported TOML table at line %d", i+1)
			}
			current = root
			for _, raw := range splitConfig(line[1:len(line)-1], '.') {
				key, err := configKey(raw)
				if err != nil {
					return nil, err
				}
				if current[key] == nil {
					current[key] = map[string]any{}
				}
				next, ok := current[key].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("TOML table conflicts with %s", key)
				}
				current = next
			}
			continue
		}
		pieces := splitConfig(line, '=')
		if len(pieces) < 2 {
			return nil, fmt.Errorf("invalid TOML at line %d", i+1)
		}
		key, err := configKey(pieces[0])
		if err != nil {
			return nil, err
		}
		raw := strings.TrimSpace(strings.Join(pieces[1:], "="))
		var value any
		if strings.HasPrefix(raw, "\"\"\"") || strings.HasPrefix(raw, "'''") {
			delim := raw[:3]
			raw = raw[3:]
			if raw == "" {
				i++
				if i >= len(lines) {
					return nil, fmt.Errorf("unterminated TOML string")
				}
				raw = lines[i]
			}
			for !strings.Contains(raw, delim) {
				i++
				if i >= len(lines) {
					return nil, fmt.Errorf("unterminated TOML string")
				}
				raw += "\n" + lines[i]
			}
			end := strings.Index(raw, delim)
			if strings.TrimSpace(configComment(raw[end+3:])) != "" {
				return nil, fmt.Errorf("unexpected content after TOML string")
			}
			raw = raw[:end]
			if delim == "\"\"\"" {
				decoded, err := decodeTOMLMultiline(raw)
				if err != nil {
					return nil, err
				}
				value = decoded
			} else {
				value = raw
			}
		} else {
			// Join multiline arrays, including comments on their own lines.
			for strings.HasPrefix(raw, "[") && !strings.HasSuffix(raw, "]") {
				i++
				if i >= len(lines) {
					return nil, fmt.Errorf("unterminated TOML array")
				}
				raw += " " + configComment(lines[i])
			}
			value, err = configValue(raw)
			if err != nil {
				return nil, err
			}
		}
		if _, exists := current[key]; exists {
			return nil, fmt.Errorf("duplicate TOML key %q", key)
		}
		current[key] = value
	}
	return root, nil
}

func decodeTOMLMultiline(raw string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			out.WriteByte(raw[i])
			continue
		}
		if i+1 >= len(raw) {
			return "", fmt.Errorf("unterminated TOML escape")
		}
		i++
		if raw[i] == '\n' || raw[i] == '\r' {
			for i+1 < len(raw) && strings.ContainsRune(" \t\r\n", rune(raw[i+1])) {
				i++
			}
			continue
		}
		end := i + 1
		if raw[i] == 'u' {
			end = i + 5
		}
		if raw[i] == 'U' {
			end = i + 9
		}
		if end > len(raw) {
			return "", fmt.Errorf("short TOML Unicode escape")
		}
		value, err := strconv.Unquote("\"\\" + raw[i:end] + "\"")
		if err != nil {
			return "", err
		}
		out.WriteString(value)
		i = end - 1
	}
	return out.String(), nil
}

func parseDefinitionYAML(text string) (map[string]any, error) {
	var jsonMap map[string]any
	if json.Unmarshal([]byte(text), &jsonMap) == nil {
		return jsonMap, nil
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	index := 0
	indent := func(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }
	var block func(int) (any, error)
	block = func(level int) (any, error) {
		if level > 256 {
			return nil, fmt.Errorf("YAML nesting exceeds 256 columns")
		}
		object := map[string]any{}
		var list []any
		isList := false
		haveValue := false
		for index < len(lines) {
			raw := lines[index]
			trimmed := configComment(raw)
			if trimmed == "" || trimmed == "---" || trimmed == "..." {
				index++
				continue
			}
			if strings.HasPrefix(raw, "\t") {
				return nil, fmt.Errorf("YAML tab indentation at line %d", index+1)
			}
			depth := indent(raw)
			if depth < level {
				break
			}
			if depth > level {
				return nil, fmt.Errorf("unexpected YAML indentation at line %d", index+1)
			}
			if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
				if haveValue && !isList {
					return nil, fmt.Errorf("mixed YAML mapping and sequence")
				}
				isList = true
				haveValue = true
				item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
				if strings.Contains(item, ": ") && !strings.HasPrefix(item, "\"") && !strings.HasPrefix(item, "'") && !strings.HasPrefix(item, "{") {
					return nil, fmt.Errorf("YAML sequences of mappings are not supported")
				}
				value, err := configValue(item)
				if err != nil {
					return nil, err
				}
				list = append(list, value)
				index++
				continue
			}
			if isList {
				return nil, fmt.Errorf("mixed YAML mapping and sequence")
			}
			haveValue = true
			parts := splitConfig(trimmed, ':')
			if len(parts) < 2 {
				return nil, fmt.Errorf("invalid YAML mapping at line %d", index+1)
			}
			key, err := configKey(parts[0])
			if err != nil {
				return nil, err
			}
			if _, ok := object[key]; ok {
				return nil, fmt.Errorf("duplicate YAML key %q", key)
			}
			valueText := strings.TrimSpace(strings.Join(parts[1:], ":"))
			index++
			var value any
			if valueText == "" {
				for index < len(lines) && configComment(lines[index]) == "" {
					index++
				}
				if index < len(lines) && indent(lines[index]) > level {
					value, err = block(indent(lines[index]))
				} else {
					value = nil
				}
			} else if strings.HasPrefix(valueText, "|") || strings.HasPrefix(valueText, ">") {
				if valueText != "|" && valueText != "|-" && valueText != "|+" && valueText != ">" && valueText != ">-" {
					return nil, fmt.Errorf("unsupported YAML block indicator %q", valueText)
				}
				start, contentIndent := index, -1
				for index < len(lines) {
					if strings.TrimSpace(lines[index]) != "" {
						if indent(lines[index]) <= level {
							break
						}
						if contentIndent < 0 || indent(lines[index]) < contentIndent {
							contentIndent = indent(lines[index])
						}
					}
					index++
				}
				var body []string
				for _, line := range lines[start:index] {
					if len(line) >= contentIndent && contentIndent >= 0 {
						body = append(body, line[contentIndent:])
					} else {
						body = append(body, "")
					}
				}
				separator := "\n"
				if valueText[0] == '>' {
					separator = " "
				}
				joined := strings.Join(body, separator)
				if !strings.HasSuffix(valueText, "-") {
					joined = strings.TrimRight(joined, "\n") + "\n"
				}
				value = joined
			} else {
				value, err = configValue(valueText)
			}
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if isList {
			return list, nil
		}
		return object, nil
	}
	value, err := block(0)
	if err != nil {
		return nil, err
	}
	out, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("agent configuration must be a mapping")
	}
	return out, nil
}
