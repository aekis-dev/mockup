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
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
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

// MergeMapping combines multiple template.Mappings into one — first match wins.
// Use this to merge a LookupRegistry mapping with your own vars before calling
// Render or Project.
func MergeMapping(mappings ...template.Mapping) template.Mapping {
	return func(key string) (string, bool) {
		for _, m := range mappings {
			if v, ok := m(key); ok {
				return v, ok
			}
		}
		return "", false
	}
}

// LookupRegistry accumulates runtime values registered during template
// expansion via {{ lookup .Field }}. Each value gets a generated key emitted
// as a ${__LOOKUP_N__} placeholder in the YAML output, resolved by
// interpolate() in phase 2 via the Mapping it exposes.
type LookupRegistry struct {
	mu      sync.Mutex
	entries map[string]string
	counter atomic.Int64
}

// NewLookupRegistry returns an empty registry ready for use.
func NewLookupRegistry() *LookupRegistry {
	return &LookupRegistry{entries: make(map[string]string)}
}

// register stores value under a generated key and returns the ${KEY}
// placeholder string to be emitted into the YAML.
func (r *LookupRegistry) register(value string) string {
	key := fmt.Sprintf("__LOOKUP_%d__", r.counter.Add(1)-1)
	r.mu.Lock()
	r.entries[key] = value
	r.mu.Unlock()
	return "${" + key + "}"
}

// Mapping returns a compose-go template.Mapping that resolves all registered
// lookup keys. Pass this to MergeMapping alongside your own vars.
func (r *LookupRegistry) Mapping() template.Mapping {
	r.mu.Lock()
	snapshot := make(map[string]string, len(r.entries))
	for k, v := range r.entries {
		snapshot[k] = v
	}
	r.mu.Unlock()
	return func(key string) (string, bool) {
		v, ok := snapshot[key]
		return v, ok
	}
}

// funcMap provides the template functions available to all mockups.
//
//   - placeholder "VAR" — emits ${VAR}; use when the var name is a literal
//     known at template-write time.
//   - lookup is registered as a no-op stub so templates parse without error
//     before Expand binds the real registry-backed implementation.
var funcMap = text_template.FuncMap{
	"placeholder": func(name string) string { return "${" + name + "}" },
	"lookup":      func(value string) string { return value },
}

// Mockup holds a parsed compose mockup. Every mockup is backed by a
// text/template regardless of whether it uses dynamic directives, so plain
// YAML files and template files are handled identically.
//
// Two-phase rendering:
//  1. Expand executes the text/template with Go data and a LookupRegistry,
//     producing YAML that still contains ${VAR} placeholders.
//     {{ placeholder "VAR" }} emits ${VAR} for vars known at write time.
//     {{ lookup .Field }} registers a runtime value and emits ${__LOOKUP_N__}.
//  2. Render / Project substitutes all ${VAR} placeholders — including any
//     ${__LOOKUP_N__} keys — via interpolate(), covering both keys and values.
type Mockup struct {
	name string
	tmpl *text_template.Template
	raw  map[string]interface{} // populated by Expand
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

// Expand executes the template with data, wiring {{ lookup }} to register
// values into reg, and parses the resulting YAML into m.raw — making the
// mockup ready for Render or Project.
// For mockups with no dynamic directives or lookup calls, pass nil as data
// and a fresh NewLookupRegistry().
func (m *Mockup) Expand(data any, reg *LookupRegistry) error {
	// Clone so the base template is not mutated across concurrent expansions.
	tmpl, err := m.tmpl.Clone()
	if err != nil {
		return fmt.Errorf("clone mockup %s: %w", m.name, err)
	}
	tmpl.Funcs(text_template.FuncMap{
		"lookup": reg.register,
	})
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("expand mockup %s: %w", m.name, err)
	}
	var raw map[string]interface{}
	if err := yaml.Unmarshal(buf.Bytes(), &raw); err != nil {
		return fmt.Errorf("parse expanded mockup %s: %w", m.name, err)
	}
	if _, ok := raw["services"]; !ok {
		return fmt.Errorf("mockup %s missing top-level 'services' key", m.name)
	}
	m.raw = raw
	return nil
}

// Render substitutes vars into the mockup — both keys and values — and
// returns the resulting map. Multi-line string values like PEM certs are
// preserved correctly since they remain as Go strings throughout.
func (m *Mockup) Render(mapping template.Mapping) (map[string]interface{}, error) {
	if m.raw == nil {
		return nil, fmt.Errorf("mockup %s: Render called before Expand", m.name)
	}
	result, err := interpolate(m.raw, mapping)
	if err != nil {
		return nil, fmt.Errorf("render mockup %s: %w", m.name, err)
	}
	rendered, ok := result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("render mockup %s: unexpected root type", m.name)
	}
	return rendered, nil
}

// Project renders the mockup, loads it in-memory via loader.LoadWithContext,
// writes the canonical compose.yaml once to <dir>/<composeName>/compose.yaml,
// then reloads from that stable path for the compose SDK.
func (m *Mockup) Project(goCtx context.Context, dir string, mapping template.Mapping) (*types.Project, error) {
	composeName, _ := mapping("COMPOSE_NAME")
	if composeName == "" {
		return nil, fmt.Errorf("mockup %s: COMPOSE_NAME var is required", m.name)
	}

	// Step 1 — render: interpolate both keys and values in memory
	rendered, err := m.Render(mapping)
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

// StartCompose starts all containers belonging to a compose project by label.
// Used as a replacement for the compose SDK's Start which has issues finding
// containers that were just created via compose Create.
func StartCompose(ctx context.Context, docker *client.Client, projectName string) error {
	f := filters.NewArgs()
	f.Add("label", "com.docker.compose.project="+projectName)

	list, err := docker.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: f,
	})
	if err != nil {
		return fmt.Errorf("list compose containers for %s: %w", projectName, err)
	}
	if len(list) == 0 {
		return fmt.Errorf("no containers found for compose project %s", projectName)
	}

	for _, ct := range list {
		if ct.State == "running" {
			continue
		}
		if err := docker.ContainerStart(ctx, ct.ID, container.StartOptions{}); err != nil {
			return fmt.Errorf("start container %s: %w", ct.ID[:12], err)
		}
	}
	return nil
}
