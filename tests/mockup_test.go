package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aekis-dev/mockup"
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

// mustRender calls Render and fails the test on error.
func mustRender(t *testing.T, m *mockup.Mockup, vars map[string]interface{}) map[string]interface{} {
	t.Helper()
	rendered, err := m.Render(vars)
	if err != nil {
		t.Fatalf("Render: unexpected error: %v", err)
	}
	return rendered
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
	// Parse itself does not validate services — Render does.
	// So Parse should succeed; Render should fail.
	yaml := []byte(`
name: myapp
networks:
  default:
    external: true
`)
	m, err := mockup.Parse("test", yaml)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	_, err = m.Render(nil)
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
	_, err = m.Render(nil)
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

// ─── Render ───────────────────────────────────────────────────────────────────

func TestRender_PlainYAMLPassthrough(t *testing.T) {
	// Plain YAML with no template directives — Render with nil vars should
	// parse cleanly and return the service map unchanged.
	m := mustParse(t, "test", []byte(minimalYAML))
	rendered := mustRender(t, m, nil)

	services, ok := rendered["services"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected services map, got %T", rendered["services"])
	}
	if _, ok := services["app"]; !ok {
		t.Error("expected 'app' service in rendered output")
	}
}

func TestRender_TemplateWithData(t *testing.T) {
	yaml := []byte(`
services:
{{ range .Services }}
  {{ .Name }}:
    image: {{ .Image }}
{{ end }}
`)
	type svc struct{ Name, Image string }

	rendered := mustRender(t, mustParse(t, "test", yaml), map[string]interface{}{
		"Services": []svc{
			{Name: "alpha", Image: "alpine:3"},
			{Name: "beta", Image: "nginx:latest"},
		},
	})

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

func TestRender_Placeholder(t *testing.T) {
	// {{ placeholder "VAR" }} must survive phase 1 as ${VAR} and be
	// substituted in phase 2 via the string value in vars.
	yaml := []byte(`
services:
  app:
    image: {{ placeholder "IMAGE" }}
`)
	rendered := mustRender(t, mustParse(t, "test", yaml), map[string]interface{}{
		"IMAGE": "alpine:edge",
	})

	svc := rendered["services"].(map[string]interface{})["app"].(map[string]interface{})
	if svc["image"] != "alpine:edge" {
		t.Errorf("expected image 'alpine:edge', got %v", svc["image"])
	}
}

func TestRender_LookupRegistersValue(t *testing.T) {
	// {{ lookup .Secret }} must register the runtime value and resolve it
	// transparently in phase 2 without any caller involvement.
	yaml := []byte(`
services:
  app:
    image: alpine
    environment:
      SECRET: {{ lookup .Secret }}
`)
	rendered := mustRender(t, mustParse(t, "test", yaml), map[string]interface{}{
		"Secret": "s3cr3t-value",
	})

	env := rendered["services"].(map[string]interface{})["app"].(map[string]interface{})["environment"].(map[string]interface{})
	if env["SECRET"] != "s3cr3t-value" {
		t.Errorf("expected SECRET 's3cr3t-value', got %v", env["SECRET"])
	}
}

func TestRender_MultipleLookupKeys(t *testing.T) {
	// Each {{ lookup }} call must produce a distinct __LOOKUP_N__ key and
	// both must resolve correctly in phase 2.
	yaml := []byte(`
services:
  app:
    image: alpine
    environment:
      CERT: {{ lookup .Cert }}
      KEY: {{ lookup .Key }}
`)
	rendered := mustRender(t, mustParse(t, "test", yaml), map[string]interface{}{
		"Cert": "cert-pem",
		"Key":  "key-pem",
	})

	env := rendered["services"].(map[string]interface{})["app"].(map[string]interface{})["environment"].(map[string]interface{})
	if env["CERT"] != "cert-pem" {
		t.Errorf("expected CERT 'cert-pem', got %v", env["CERT"])
	}
	if env["KEY"] != "key-pem" {
		t.Errorf("expected KEY 'key-pem', got %v", env["KEY"])
	}
}

func TestRender_SubstitutesValues(t *testing.T) {
	yaml := []byte(`
services:
  app:
    image: ${IMAGE}
    restart: ${RESTART}
`)
	rendered := mustRender(t, mustParse(t, "test", yaml), map[string]interface{}{
		"IMAGE":   "alpine:3",
		"RESTART": "always",
	})

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
	rendered := mustRender(t, mustParse(t, "test", yaml), map[string]interface{}{
		"CONFIG_NAME": "my-config",
	})

	configs, ok := rendered["configs"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected configs map, got %T", rendered["configs"])
	}
	if _, ok := configs["my-config"]; !ok {
		t.Errorf("expected key 'my-config' in configs after substitution, got keys: %v", configs)
	}
}

func TestRender_LookupAndVarsTogether(t *testing.T) {
	// End-to-end: string vars resolve via ${VAR}, non-string vars feed the
	// template, and {{ lookup }} values are registered and resolved internally.
	yaml := []byte(`
services:
  app:
    image: ${IMAGE}
    environment:
      CA: {{ lookup .CA }}
`)
	rendered := mustRender(t, mustParse(t, "test", yaml), map[string]interface{}{
		"IMAGE": "alpine:3",
		"CA":    "-----BEGIN CERTIFICATE-----\n...",
	})

	svc := rendered["services"].(map[string]interface{})["app"].(map[string]interface{})
	if svc["image"] != "alpine:3" {
		t.Errorf("expected image 'alpine:3', got %v", svc["image"])
	}
	env := svc["environment"].(map[string]interface{})
	if env["CA"] != "-----BEGIN CERTIFICATE-----\n..." {
		t.Errorf("expected CA cert value, got %v", env["CA"])
	}
}

func TestRender_MissingVar_LeavesEmpty(t *testing.T) {
	// compose-go's template.Substitute resolves unknown vars to an empty
	// string — no error, just unresolved placeholder becomes "".
	yaml := []byte(`
services:
  app:
    image: ${MISSING}
`)
	rendered := mustRender(t, mustParse(t, "test", yaml), nil)

	svc := rendered["services"].(map[string]interface{})["app"].(map[string]interface{})
	if svc["image"] != "" {
		t.Errorf("expected empty string for unresolved var, got %v", svc["image"])
	}
}
