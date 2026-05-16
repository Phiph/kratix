package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func newTestInput() hsaInput {
	in := hsaInput{}
	in.APIVersion = "agents.kratix.io/v1alpha1"
	in.Kind = "HealthSummaryAgent"
	in.Metadata.Name = "primary"
	in.Metadata.Namespace = "observability"
	in.Spec.DigestSchedule = "@hourly"
	in.Spec.Subscribe = []string{"kratix.promise.*", "kratix.work.*"}
	in.Spec.Image = "ghcr.io/syntasso/health-summary-agent:v0.1"
	return in
}

func TestBuild_ProducesAllExpectedFiles(t *testing.T) {
	files := build(newTestInput())
	want := map[string]bool{
		"configmap.yaml":      false,
		"serviceaccount.yaml": false,
		"rbac.yaml":           false,
		"deployment.yaml":     false,
		"service.yaml":        false,
		"cloudeventsink.yaml": false,
	}
	for _, f := range files {
		if _, ok := want[f.filename]; !ok {
			t.Errorf("unexpected file: %s", f.filename)
			continue
		}
		want[f.filename] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing file: %s", name)
		}
	}
}

func TestBuildSink_DerivesURL(t *testing.T) {
	files := build(newTestInput())
	sink := findResource(t, files, "cloudeventsink.yaml")
	spec := sink["spec"].(map[string]any)
	url := spec["url"].(string)
	want := "http://health-summary-agent-primary.observability.svc:8080/events"
	if url != want {
		t.Errorf("sink URL = %q, want %q", url, want)
	}
	filter := spec["typeFilter"].([]any)
	if len(filter) != 2 {
		t.Errorf("typeFilter len = %d, want 2", len(filter))
	}
}

func TestBuildSink_NameDisambiguatedByNamespace(t *testing.T) {
	// CloudEventSink is cluster-scoped. Two HSAs of the same name in
	// different namespaces must not collide on Sink names.
	in := newTestInput()
	files := build(in)
	sink := findResource(t, files, "cloudeventsink.yaml")
	name := sink["metadata"].(map[string]any)["name"].(string)
	if name != "health-summary-agent-primary-observability" {
		t.Errorf("sink name = %q; expected disambiguation by namespace", name)
	}
}

func TestBuildDeployment_BasicShape(t *testing.T) {
	files := build(newTestInput())
	dep := findResource(t, files, "deployment.yaml")
	spec := dep["spec"].(map[string]any)
	template := spec["template"].(map[string]any)
	podSpec := template["spec"].(map[string]any)

	if sa := podSpec["serviceAccountName"]; sa != "health-summary-agent-primary" {
		t.Errorf("serviceAccountName = %v", sa)
	}

	containers := podSpec["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	c := containers[0].(map[string]any)
	if img := c["image"]; img != "ghcr.io/syntasso/health-summary-agent:v0.1" {
		t.Errorf("image = %v", img)
	}
}

func TestReadInput_AppliesDefaults(t *testing.T) {
	yamlBody := `
apiVersion: agents.kratix.io/v1alpha1
kind: HealthSummaryAgent
metadata:
  name: minimal
  namespace: default
spec: {}
`
	tmp := t.TempDir()
	path := filepath.Join(tmp, "object.yaml")
	if err := os.WriteFile(path, []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := readInput(path)
	if err != nil {
		t.Fatalf("readInput: %v", err)
	}
	if in.Spec.DigestSchedule != defaultSchedule {
		t.Errorf("DigestSchedule = %q, want default %q", in.Spec.DigestSchedule, defaultSchedule)
	}
	if len(in.Spec.Subscribe) != 1 || in.Spec.Subscribe[0] != "kratix.*" {
		t.Errorf("Subscribe = %v, want [kratix.*]", in.Spec.Subscribe)
	}
	if in.Spec.Image != defaultImage {
		t.Errorf("Image = %q, want default", in.Spec.Image)
	}
}

func TestWriteAll_RBACWritesMultiDoc(t *testing.T) {
	// rbac.yaml is the only file emitted as a multi-document YAML stream
	// (Role + RoleBinding). Pin that behaviour — the work-creator handles
	// multi-doc YAML, but losing the separator would silently drop the
	// second resource.
	tmp := t.TempDir()
	files := build(newTestInput())
	if err := writeAll(tmp, files); err != nil {
		t.Fatal(err)
	}
	rbac, err := os.ReadFile(filepath.Join(tmp, "rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// One document per YAML stream entry. Decode the file and count.
	dec := yaml.NewDecoder(bytes.NewReader(rbac))
	docs := 0
	for {
		var v any
		if err := dec.Decode(&v); err != nil {
			break
		}
		docs++
	}
	if docs != 2 {
		t.Errorf("expected 2 documents in rbac.yaml, got %d", docs)
	}
}

func findResource(t *testing.T, files []resourceFile, name string) map[string]any {
	t.Helper()
	for _, f := range files {
		if f.filename == name {
			return f.body
		}
	}
	t.Fatalf("file %q not in build output", name)
	return nil
}

// Confirm yaml round-trip works for all bodies — the pipeline is useless
// if the work-creator can't parse what it writes.
func TestWriteAll_AllFilesParseAsYAML(t *testing.T) {
	tmp := t.TempDir()
	if err := writeAll(tmp, build(newTestInput())); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(tmp, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var v any
			if err := dec.Decode(&v); err != nil {
				if err.Error() == "EOF" {
					break
				}
				t.Errorf("%s: yaml decode: %v", e.Name(), err)
				break
			}
		}
	}
}
