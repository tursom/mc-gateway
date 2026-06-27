package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/tursom/mc-gateway/internal/pluginmanager"
	"gopkg.in/yaml.v3"
)

const (
	manifestFormatJSON  = "json"
	manifestFormatJSONC = "jsonc"
	manifestFormatYAML  = "yaml"
	manifestFormatTOML  = "toml"
)

var manifestSourceNames = []string{
	"manifest.yaml",
	"manifest.yml",
	"manifest.toml",
	"manifest.jsonc",
	"manifest.json",
}

type pluginManifestSource struct {
	Path          string
	Format        string
	Data          []byte
	Manifest      pluginmanager.Manifest
	Raw           map[string]any
	CanonicalJSON []byte
}

func readPluginDirManifest(dir, explicitManifestPath string) (pluginmanager.Manifest, map[string]any, error) {
	source, err := readPluginManifestSource(dir, explicitManifestPath)
	if err != nil {
		return pluginmanager.Manifest{}, nil, err
	}
	return source.Manifest, source.Raw, nil
}

func readPluginManifestSource(target, explicitManifestPath string) (pluginManifestSource, error) {
	manifestPath, err := resolveManifestSourcePath(target, explicitManifestPath)
	if err != nil {
		return pluginManifestSource{}, err
	}
	return readPluginManifestSourceFile(manifestPath)
}

func resolveManifestSourcePath(target, explicitManifestPath string) (string, error) {
	if explicitManifestPath != "" {
		return resolveExplicitManifestPath(target, explicitManifestPath)
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		if !isManifestSourceFile(target) {
			return "", fmt.Errorf("manifest path must be one of %s, got %q", strings.Join(manifestSourceNames, ", "), target)
		}
		return target, nil
	}
	var found []string
	for _, name := range manifestSourceNames {
		candidate := filepath.Join(target, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			found = append(found, candidate)
		}
	}
	if len(found) == 0 {
		return "", fmt.Errorf("plugin manifest is required: expected one of %s in %s", strings.Join(manifestSourceNames, ", "), target)
	}
	if len(found) > 1 {
		sort.Strings(found)
		return "", fmt.Errorf("multiple plugin manifests found: %s; pass --manifest to select one", strings.Join(found, ", "))
	}
	return found[0], nil
}

func resolveExplicitManifestPath(target, explicitManifestPath string) (string, error) {
	candidates := []string{explicitManifestPath}
	if info, err := os.Stat(target); err == nil && info.IsDir() && !filepath.IsAbs(explicitManifestPath) {
		candidates = []string{filepath.Join(target, explicitManifestPath), explicitManifestPath}
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.IsDir() {
			return "", fmt.Errorf("manifest path %q is a directory", candidate)
		}
		if !isManifestSourceFile(candidate) {
			return "", fmt.Errorf("manifest path must be one of %s, got %q", strings.Join(manifestSourceNames, ", "), candidate)
		}
		return candidate, nil
	}
	return "", fmt.Errorf("manifest path %q is not readable", explicitManifestPath)
}

func readPluginManifestSourceFile(manifestPath string) (pluginManifestSource, error) {
	format, err := manifestFormatForPath(manifestPath)
	if err != nil {
		return pluginManifestSource{}, err
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return pluginManifestSource{}, err
	}
	raw, err := decodeManifestSource(data, format)
	if err != nil {
		return pluginManifestSource{}, fmt.Errorf("invalid %s: %w", filepath.Base(manifestPath), err)
	}
	canonical, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return pluginManifestSource{}, err
	}
	canonical = append(canonical, '\n')
	var manifest pluginmanager.Manifest
	if err := json.Unmarshal(canonical, &manifest); err != nil {
		return pluginManifestSource{}, fmt.Errorf("invalid %s object: %w", filepath.Base(manifestPath), err)
	}
	if manifest.ID == "" {
		return pluginManifestSource{}, errors.New("manifest id is required")
	}
	return pluginManifestSource{
		Path:          manifestPath,
		Format:        format,
		Data:          data,
		Manifest:      manifest,
		Raw:           raw,
		CanonicalJSON: canonical,
	}, nil
}

func decodeManifestSource(data []byte, format string) (map[string]any, error) {
	var raw any
	switch format {
	case manifestFormatJSON:
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
	case manifestFormatJSONC:
		stripped, err := stripJSONC(data)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(stripped, &raw); err != nil {
			return nil, err
		}
	case manifestFormatYAML:
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
	case manifestFormatTOML:
		var table map[string]any
		if err := toml.Unmarshal(data, &table); err != nil {
			return nil, err
		}
		raw = table
	default:
		return nil, fmt.Errorf("unsupported manifest format %q", format)
	}
	normalized, ok := normalizeManifestValue(raw).(map[string]any)
	if !ok {
		return nil, errors.New("manifest root must be an object")
	}
	return normalized, nil
}

func normalizeManifestValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = normalizeManifestValue(item)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[fmt.Sprint(key)] = normalizeManifestValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = normalizeManifestValue(item)
		}
		return out
	default:
		return value
	}
}

func manifestFormatForPath(filePath string) (string, error) {
	switch strings.ToLower(filepath.Base(filePath)) {
	case "manifest.json":
		return manifestFormatJSON, nil
	case "manifest.jsonc":
		return manifestFormatJSONC, nil
	case "manifest.yaml", "manifest.yml":
		return manifestFormatYAML, nil
	case "manifest.toml":
		return manifestFormatTOML, nil
	default:
		return "", fmt.Errorf("unsupported manifest file %q", filePath)
	}
}

func isManifestSourceFile(filePath string) bool {
	_, err := manifestFormatForPath(filePath)
	return err == nil
}

func stripJSONC(data []byte) ([]byte, error) {
	withoutComments, err := stripJSONCComments(data)
	if err != nil {
		return nil, err
	}
	return stripJSONCTrailingCommas(withoutComments), nil
}

func stripJSONCComments(data []byte) ([]byte, error) {
	out := make([]byte, 0, len(data))
	inString := false
	escaped := false
	for i := 0; i < len(data); i++ {
		ch := data[i]
		if inString {
			out = append(out, ch)
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			out = append(out, ch)
			continue
		}
		if ch == '/' && i+1 < len(data) {
			next := data[i+1]
			if next == '/' {
				out = append(out, ' ', ' ')
				i += 2
				for ; i < len(data); i++ {
					if data[i] == '\n' || data[i] == '\r' {
						out = append(out, data[i])
						break
					}
					out = append(out, ' ')
				}
				continue
			}
			if next == '*' {
				out = append(out, ' ', ' ')
				i += 2
				closed := false
				for ; i < len(data); i++ {
					if data[i] == '*' && i+1 < len(data) && data[i+1] == '/' {
						out = append(out, ' ', ' ')
						i++
						closed = true
						break
					}
					if data[i] == '\n' || data[i] == '\r' {
						out = append(out, data[i])
					} else {
						out = append(out, ' ')
					}
				}
				if !closed {
					return nil, errors.New("unterminated block comment")
				}
				continue
			}
		}
		out = append(out, ch)
	}
	if inString {
		return nil, errors.New("unterminated string")
	}
	return out, nil
}

func stripJSONCTrailingCommas(data []byte) []byte {
	var out bytes.Buffer
	inString := false
	escaped := false
	for i := 0; i < len(data); i++ {
		ch := data[i]
		if inString {
			out.WriteByte(ch)
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			out.WriteByte(ch)
			continue
		}
		if ch == ',' {
			j := i + 1
			for j < len(data) && isJSONWhitespace(data[j]) {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue
			}
		}
		out.WriteByte(ch)
	}
	return out.Bytes()
}

func isJSONWhitespace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'
}
