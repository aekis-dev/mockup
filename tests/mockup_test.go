package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aekis-dev/mockup"
	"github.com/compose-spec/compose-go/v2/template"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// mustParse parses bytes and fails the test on error.
func mustParse(t *testing.T, name string, data []byte) *mockup.Mockup {
	t.Helper()
	m, err := mockup.Parse(name, data)
	if err != nil {
		t.Fatalf("Parse(%q): unexpected error: %v", name, err)
	}
	return m
}

// mustExpand calls Expand and fails the test on error.
func mustExpand(t *testing.T, m *mockup.Mockup, data any, reg *mockup.LookupRegistry) {
	t.Helper()
	if err := m.Expand(data, reg); err != nil {
		t.Fatalf("Expand: unexpected error: %v", err)
	}
}

// staticMapping returns a template.Mapping backed by a plain map.
func staticMapping(vars map[string]string) template.Mapping {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

// minimalYAML is the smallest valid mockup — one service, no placeholders.
const minimalYAML = `
services:
  app:
    image: alpine
`

// ─── Parse / Load ─────────────────────────────────────────────────────────────

func TestParse_ValidYAML(t *testing.T) {
	m, err := mockup.Parse("test", []byte(minimalYAML))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil Mockup")
	}
}

func TestParse_MissingServicesKey(t *testing.T) {
	yaml := []byte(`
name: myapp
networks:
  default:
    external: true
`)
	// Parse itself does not validate services — Expand does.
	// So Parse should succeed; Expand should fail.
	m, err := mockup.Parse("test", yaml)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	reg := mockup.NewLookupRegistry()
	err = m.Expand(nil, reg)
	if err == nil {
		t.Fatal("expected error for missing 'services' key, got nil")
	}
	if !strings.Contains(err.Error(), "services") {
		t.Errorf("expected error to mention 'services', got: %v", err)
	}
}

func TestParse_InvalidYAML(t *testing.T) {
	// Valid template syntax but produces invalid YAML after execution.
	yaml := []byte(`
services:
  app:
    image: [unclosed bracket
`)
	m, err := mockup.Parse("test", yaml)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	reg := mockup.NewLookupRegistry()
	err = m.Expand(nil, reg)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestParse_InvalidTemplateSyntax(t *testing.T) {
	// Unclosed action — text/template parse error.
	yaml := []byte(`
services:
  {{ range .Items
    name: broken
`)
	_, err := mockup.Parse("test", yaml)
	if err == nil {
		t.Fatal("expected error for invalid template syntax, got nil")
	}
	if !strings.Contains(err.Error(), "parse mockup") {
		t.Errorf("expected error to mention 'parse mockup', got: %v", err)
	}
}

func TestLoad_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, []byte(minimalYAML), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	m, err := mockup.Load(path)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil Mockup")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := mockup.Load("/nonexistent/path/compose.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "read mockup") {
		t.Errorf("expected error to mention 'read mockup', got: %v", err)
	}
}

// ─── Expand ───────────────────────────────────────────────────────────────────

func TestExpand_PlainYAMLPassthrough(t *testing.T) {
	// Plain YAML with no template directives — Expand with nil data should
	// parse cleanly and populate raw.
	m := mustParse(t, "test", []byte(minimalYAML))
	reg := mockup.NewLookupRegistry()
	mustExpand(t, m, nil, reg)

	rendered, err := m.Render(staticMapping(nil))
	if err != nil {
		t.Fatalf("Render: unexpected error: %v", err)
	}
	services, ok := rendered["services"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected services map, got %T", rendered["services"])
	}
	if _, ok := services["app"]; !ok {
		t.Error("expected 'app' service in rendered output")
	}
}

func TestExpand_TemplateWithData(t *testing.T) {
	yaml := []byte(`
services:
{{ range .Services }}
  {{ .Name }}:
    image: {{ .Image }}
{{ end }}
`)
	type svc struct{ Name, Image string }
	type data struct{ Services []svc }

	m := mustParse(t, "test", yaml)
	reg := mockup.NewLookupRegistry()
	mustExpand(t, m, data{
		Services: []svc{
			{Name: "alpha", Image: "alpine:3"},
			{Name: "beta", Image: "nginx:latest"},
		},
	}, reg)

	rendered, err := m.Render(staticMapping(nil))
	if err != nil {
		t.Fatalf("Render: unexpected error: %v", err)
	}
	services, ok := rendered["services"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected services map, got %T", rendered["services"])
	}
	for _, name := range []string{"alpha", "beta"} {
		if _, ok := services[name]; !ok {
			t.Errorf("expected service %q in rendered output", name)
		}
	}
}

func TestExpand_Placeholder(t *testing.T) {
	// {{ placeholder "VAR" }} must survive Expand as ${VAR} for phase-2
	// interpolation.
	yaml := []byte(`
services:
  app:
    image: {{ placeholder "IMAGE" }}
`)
	m := mustParse(t, "test", yaml)
	reg := mockup.NewLookupRegistry()
	mustExpand(t, m, nil, reg)

	// Render with the var resolved — should see the substituted value.
	rendered, err := m.Render(staticMapping(map[string]string{
		"IMAGE": "alpine:edge",
	}))
	if err != nil {
		t.Fatalf("Render: unexpected error: %v", err)
	}
	svc := rendered["services"].(map[string]interface{})["app"].(map[string]interface{})
	if svc["image"] != "alpine:edge" {
		t.Errorf("expected image 'alpine:edge', got %v", svc["image"])
	}
}

func TestExpand_LookupRegistersValue(t *testing.T) {
	// {{ lookup .Secret }} must register the runtime value and emit a
	// ${__LOOKUP_N__} placeholder that resolves via reg.Mapping().
	yaml := []byte(`
services:
  app:
    image: alpine
    environment:
      SECRET: {{ lookup .Secret }}
`)
	type data struct{ Secret string }

	m := mustParse(t, "test", yaml)
	reg := mockup.NewLookupRegistry()
	mustExpand(t, m, data{Secret: "s3cr3t-value"}, reg)

	mapping := mockup.MergeMapping(reg.Mapping(), staticMapping(nil))
	rendered, err := m.Render(mapping)
	if err != nil {
		t.Fatalf("Render: unexpected error: %v", err)
	}
	svc := rendered["services"].(map[string]interface{})["app"].(map[string]interface{})
	env := svc["environment"].(map[string]interface{})
	if env["SECRET"] != "s3cr3t-value" {
		t.Errorf("expected SECRET 's3cr3t-value', got %v", env["SECRET"])
	}
}

func TestExpand_MultipleLookupKeys(t *testing.T) {
	// Each {{ lookup }} call must produce a distinct __LOOKUP_N__ key.
	yaml := []byte(`
services:
  app:
    image: alpine
    environment:
      CERT: {{ lookup .Cert }}
      KEY: {{ lookup .Key }}
`)
	type data struct{ Cert, Key string }

	m := mustParse(t, "test", yaml)
	reg := mockup.NewLookupRegistry()
	mustExpand(t, m, data{Cert: "cert-pem", Key: "key-pem"}, reg)

	mapping := mockup.MergeMapping(reg.Mapping(), staticMapping(nil))
	rendered, err := m.Render(mapping)
	if err != nil {
		t.Fatalf("Render: unexpected error: %v", err)
	}
	env := rendered["services"].(map[string]interface{})["app"].(map[string]interface{})["environment"].(map[string]interface{})
	if env["CERT"] != "cert-pem" {
		t.Errorf("expected CERT 'cert-pem', got %v", env["CERT"])
	}
	if env["KEY"] != "key-pem" {
		t.Errorf("expected KEY 'key-pem', got %v", env["KEY"])
	}
}

// ─── Render ───────────────────────────────────────────────────────────────────

func TestRender_BeforeExpand(t *testing.T) {
	m := mustParse(t, "test", []byte(minimalYAML))
	_, err := m.Render(staticMapping(nil))
	if err == nil {
		t.Fatal("expected error when Render called before Expand, got nil")
	}
	if !strings.Contains(err.Error(), "Render called before Expand") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRender_SubstitutesValues(t *testing.T) {
	yaml := []byte(`
services:
  app:
    image: ${IMAGE}
    restart: ${RESTART}
`)
	m := mustParse(t, "test", yaml)
	mustExpand(t, m, nil, mockup.NewLookupRegistry())

	rendered, err := m.Render(staticMapping(map[string]string{
		"IMAGE":   "alpine:3",
		"RESTART": "always",
	}))
	if err != nil {
		t.Fatalf("Render: unexpected error: %v", err)
	}
	svc := rendered["services"].(map[string]interface{})["app"].(map[string]interface{})
	if svc["image"] != "alpine:3" {
		t.Errorf("expected image 'alpine:3', got %v", svc["image"])
	}
	if svc["restart"] != "always" {
		t.Errorf("expected restart 'always', got %v", svc["restart"])
	}
}

func TestRender_SubstitutesKeys(t *testing.T) {
	// ${VAR} in a map key must also be substituted.
	yaml := []byte(`
services:
  app:
    image: alpine
configs:
  ${CONFIG_NAME}:
    name: ${CONFIG_NAME}
    content: hello
`)
	m := mustParse(t, "test", yaml)
	mustExpand(t, m, nil, mockup.NewLookupRegistry())

	rendered, err := m.Render(staticMapping(map[string]string{
		"CONFIG_NAME": "my-config",
	}))
	if err != nil {
		t.Fatalf("Render: unexpected error: %v", err)
	}
	configs, ok := rendered["configs"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected configs map, got %T", rendered["configs"])
	}
	if _, ok := configs["my-config"]; !ok {
		t.Errorf("expected key 'my-config' in configs after substitution, got keys: %v", configs)
	}
}

func TestRender_LookupResolvesViaMergeMapping(t *testing.T) {
	// End-to-end: lookup registers a value during Expand; reg.Mapping()
	// merged with static vars resolves both during Render.
	yaml := []byte(`
services:
  app:
    image: ${IMAGE}
    environment:
      CA: {{ lookup .CA }}
`)
	type data struct{ CA string }

	m := mustParse(t, "test", yaml)
	reg := mockup.NewLookupRegistry()
	mustExpand(t, m, data{CA: "-----BEGIN CERTIFICATE-----\n..."}, reg)

	mapping := mockup.MergeMapping(reg.Mapping(), staticMapping(map[string]string{
		"IMAGE": "alpine:3",
	}))
	rendered, err := m.Render(mapping)
	if err != nil {
		t.Fatalf("Render: unexpected error: %v", err)
	}
	svc := rendered["services"].(map[string]interface{})["app"].(map[string]interface{})
	if svc["image"] != "alpine:3" {
		t.Errorf("expected image 'alpine:3', got %v", svc["image"])
	}
	env := svc["environment"].(map[string]interface{})
	if env["CA"] != "-----BEGIN CERTIFICATE-----\n..." {
		t.Errorf("expected CA cert value, got %v", env["CA"])
	}
}

func TestRender_MissingVar_LeavesPlaceholder(t *testing.T) {
	// compose-go's template.Substitute leaves ${MISSING} as-is when the
	// mapping returns false — no error, just unresolved placeholder.
	yaml := []byte(`
services:
  app:
    image: ${MISSING}
`)
	m := mustParse(t, "test", yaml)
	mustExpand(t, m, nil, mockup.NewLookupRegistry())

	rendered, err := m.Render(staticMapping(nil))
	if err != nil {
		t.Fatalf("Render: unexpected error: %v", err)
	}
	svc := rendered["services"].(map[string]interface{})["app"].(map[string]interface{})
	// The placeholder is left as an empty string by compose-go when unresolved.
	if svc["image"] != "" {
		t.Errorf("expected empty string for unresolved var, got %v", svc["image"])
	}
}

// ─── MergeMapping ─────────────────────────────────────────────────────────────

func TestMergeMapping_FirstMatchWins(t *testing.T) {
	first := staticMapping(map[string]string{"X": "from-first"})
	second := staticMapping(map[string]string{"X": "from-second", "Y": "from-second"})
	merged := mockup.MergeMapping(first, second)

	if v, ok := merged("X"); !ok || v != "from-first" {
		t.Errorf("expected 'from-first', got %q (ok=%v)", v, ok)
	}
	if v, ok := merged("Y"); !ok || v != "from-second" {
		t.Errorf("expected 'from-second', got %q (ok=%v)", v, ok)
	}
}

func TestMergeMapping_AllMiss(t *testing.T) {
	merged := mockup.MergeMapping(staticMapping(nil), staticMapping(nil))
	if _, ok := merged("NOPE"); ok {
		t.Error("expected ok=false for missing key")
	}
}
