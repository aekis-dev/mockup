package mockup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	text_template "text/template"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/template"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

// interpolate recursively walks a map[string]interface{} substituting
// ${VAR} in both keys and values using compose-go's template.Substitute.
// This is the core difference from compose-go's interpolation package
// which only substitutes values.
func interpolate(node interface{}, m template.Mapping) (interface{}, error) {
	switch v := node.(type) {

	case map[string]interface{}:
		out := make(map[string]interface{}, len(v))
		for key, val := range v {
			// Interpolate the key itself
			newKey, err := template.Substitute(key, m)
			if err != nil {
				return nil, fmt.Errorf("interpolate key %q: %w", key, err)
			}
			// Recurse into the value
			newVal, err := interpolate(val, m)
			if err != nil {
				return nil, err
			}
			out[newKey] = newVal
		}
		return out, nil

	case []interface{}:
		out := make([]interface{}, len(v))
		for i, elem := range v {
			newElem, err := interpolate(elem, m)
			if err != nil {
				return nil, err
			}
			out[i] = newElem
		}
		return out, nil

	case string:
		newVal, err := template.Substitute(v, m)
		if err != nil {
			return nil, fmt.Errorf("interpolate value %q: %w", v, err)
		}
		return newVal, nil

	default:
		// int, bool, nil — pass through unchanged
		return v, nil
	}
}

// toMapping derives a compose-go template.Mapping from vars and a registry.
// Registry keys take priority so __LOOKUP_N__ always resolves.
// Only string values in vars are resolvable; non-string values are used
// exclusively by text/template via dot-access and skipped here.
func toMapping(vars map[string]interface{}, reg *lookupRegistry) template.Mapping {
	return func(key string) (string, bool) {
		if v, ok := reg.entries[key]; ok {
			return v, true
		}
		if v, ok := vars[key]; ok {
			if s, ok := v.(string); ok {
				return s, true
			}
		}
		return "", false
	}
}

// lookupRegistry accumulates runtime values registered during template
// expansion via {{ lookup .Field }}. Each value gets a generated key emitted
// as a ${__LOOKUP_N__} placeholder in the YAML output, resolved in phase 2
// via toMapping.
type lookupRegistry struct {
	mu      sync.Mutex
	entries map[string]string
	counter atomic.Int64
}

// newLookupRegistry returns an empty registry ready for use.
func newLookupRegistry() *lookupRegistry {
	return &lookupRegistry{entries: make(map[string]string)}
}

// register stores value under a generated key and returns the ${KEY}
// placeholder string to be emitted into the YAML.
func (r *lookupRegistry) register(value string) string {
	key := fmt.Sprintf("__LOOKUP_%d__", r.counter.Add(1)-1)
	r.mu.Lock()
	r.entries[key] = value
	r.mu.Unlock()
	return "${" + key + "}"
}

// funcMap provides the template functions available to all mockups.
//
//   - placeholder "VAR" — emits ${VAR}; use when the var name is a literal
//     known at template-write time.
//   - lookup is registered as a no-op stub so templates parse without error
//     before Render binds the real registry-backed implementation.
var funcMap = text_template.FuncMap{
	"placeholder": func(name string) string { return "${" + name + "}" },
	"lookup":      func(value string) string { return value },
}

// Mockup holds a parsed compose mockup. Every mockup is backed by a
// text/template regardless of whether it uses dynamic directives, so plain
// YAML files and template files are handled identically.
//
// Two-phase rendering happens automatically inside Render / Project:
//  1. The text/template is executed with vars as dot-accessible data.
//     {{ .Field }} accesses any value by key, including non-strings.
//     {{ placeholder "VAR" }} emits ${VAR} for vars known at write time.
//     {{ lookup .Field }} registers a runtime value and emits ${__LOOKUP_N__}.
//  2. interpolate() substitutes all ${VAR} placeholders — including any
//     ${__LOOKUP_N__} keys — covering both keys and values.
//     Only string values in vars participate in interpolation.
type Mockup struct {
	name string
	tmpl *text_template.Template
}

// Load parses a file as a Mockup.
func Load(path string) (*Mockup, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mockup %s: %w", path, err)
	}
	return Parse(filepath.Base(path), data)
}

// Parse parses raw bytes as a Mockup.
func Parse(name string, data []byte) (*Mockup, error) {
	tmpl, err := text_template.New(name).Funcs(funcMap).Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse mockup %s: %w", name, err)
	}
	return &Mockup{name: name, tmpl: tmpl}, nil
}

// Render executes the template with vars, then substitutes all ${VAR}
// placeholders — including any {{ lookup }} values registered during
// execution — in both keys and values.
// vars serves dual purpose: non-string values are accessed via {{ .Field }}
// in the template; string values are also substituted via ${VAR} in phase 2.
// For mockups with no dynamic directives, pass nil.
func (m *Mockup) Render(vars map[string]interface{}) (map[string]interface{}, error) {
	// Phase 1 — execute the template with vars as dot-accessible data.
	// Clone so the base template is not mutated across concurrent renders.
	reg := newLookupRegistry()
	tmpl, err := m.tmpl.Clone()
	if err != nil {
		return nil, fmt.Errorf("clone mockup %s: %w", m.name, err)
	}
	tmpl.Funcs(text_template.FuncMap{
		"lookup": reg.register,
	})
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return nil, fmt.Errorf("expand mockup %s: %w", m.name, err)
	}
	var raw map[string]interface{}
	if err := yaml.Unmarshal(buf.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("parse expanded mockup %s: %w", m.name, err)
	}
	if _, ok := raw["services"]; !ok {
		return nil, fmt.Errorf("mockup %s missing top-level 'services' key", m.name)
	}

	// Phase 2 — interpolate ${VAR} placeholders using string values from vars
	// and all registered __LOOKUP_N__ keys from the registry.
	result, err := interpolate(raw, toMapping(vars, reg))
	if err != nil {
		return nil, fmt.Errorf("render mockup %s: %w", m.name, err)
	}
	rendered, ok := result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("render mockup %s: unexpected root type", m.name)
	}
	return rendered, nil
}

// Project renders the mockup with vars (see Render), loads it in-memory via
// loader.LoadWithContext, writes the canonical compose.yaml once to
// <dir>/<composeName>/compose.yaml, then reloads from that stable path for
// the compose SDK. vars must contain a string key "COMPOSE_NAME".
func (m *Mockup) Project(goCtx context.Context, dir string, vars map[string]interface{}) (*types.Project, error) {
	composeName, _ := vars["COMPOSE_NAME"].(string)
	if composeName == "" {
		return nil, fmt.Errorf("mockup %s: COMPOSE_NAME var is required", m.name)
	}

	// Step 1 — render: expand template and interpolate both keys and values
	rendered, err := m.Render(vars)
	if err != nil {
		return nil, err
	}

	// Step 2 — load directly from the rendered map, no disk write needed yet.
	// types.ConfigFile.Config is used by the loader when set, skipping file I/O.
	outDir := filepath.Join(dir, composeName)
	configDetails := types.ConfigDetails{
		WorkingDir: outDir,
		ConfigFiles: []types.ConfigFile{
			{
				Filename: "compose.yaml",
				Config:   rendered,
			},
		},
	}

	project, err := loader.LoadWithContext(goCtx, configDetails,
		func(o *loader.Options) {
			o.SetProjectName(composeName, true)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("load compose project %s: %w", composeName, err)
	}

	// Step 3 — write the canonical form once to disk
	canonical, err := project.MarshalYAML()
	if err != nil {
		return nil, fmt.Errorf("marshal compose project %s: %w", composeName, err)
	}

	if err := os.MkdirAll(outDir, 0755); err != nil {
		return nil, fmt.Errorf("create compose dir %s: %w", outDir, err)
	}
	outPath := filepath.Join(outDir, "compose.yaml")
	if err := os.WriteFile(outPath, canonical, 0644); err != nil {
		return nil, fmt.Errorf("write compose file %s: %w", outPath, err)
	}

	// Step 4 — reload from the stable on-disk path so the compose SDK
	// operates against the correct project directory
	finalOptions, err := cli.NewProjectOptions(
		[]string{outPath},
		cli.WithWorkingDirectory(outDir),
		cli.WithName(composeName),
	)
	if err != nil {
		return nil, fmt.Errorf("compose project options for %s: %w", composeName, err)
	}

	project, err = finalOptions.LoadProject(goCtx)
	if err != nil {
		return nil, fmt.Errorf("reload compose project %s: %w", composeName, err)
	}

	// Ensure compose labels are set on all services.
	// The compose SDK's internal toProject() sets these but is not accessible
	// from external code — replicate them here so Create() stamps them onto containers.
	project.ComposeFiles = []string{outPath}
	for i, svc := range project.Services {
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		svc.Labels["com.docker.compose.project"] = composeName
		svc.Labels["com.docker.compose.service"] = svc.Name
		svc.Labels["com.docker.compose.project.working_dir"] = outDir
		svc.Labels["com.docker.compose.project.config_files"] = outPath
		svc.Labels["com.docker.compose.oneoff"] = "False"
		project.Services[i] = svc
	}

	return project, nil
}
