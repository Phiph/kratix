package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func newInput() scheduledJobInput {
	in := scheduledJobInput{}
	in.APIVersion = "platform.kratix.io/v1alpha1"
	in.Kind = "ScheduledJob"
	in.Metadata.Name = "nightly-cleanup"
	in.Metadata.Namespace = "team-a"
	in.Spec.Schedule = "0 3 * * *"
	in.Spec.Image = "ghcr.io/example/cleanup:v1.2.0"
	in.Spec.Args = []string{"--dry-run=false"}
	to := int64(600)
	in.Spec.TimeoutSeconds = &to
	mr := int32(2)
	in.Spec.MaxRetries = &mr
	return in
}

func TestBuild_ProducesCronJobAndAudit(t *testing.T) {
	files := build(newInput())
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	names := map[string]bool{"cronjob.yaml": false, "audit-config.yaml": false}
	for _, f := range files {
		if _, ok := names[f.name]; !ok {
			t.Errorf("unexpected file: %s", f.name)
			continue
		}
		names[f.name] = true
	}
	for n, seen := range names {
		if !seen {
			t.Errorf("missing file: %s", n)
		}
	}
}

func TestBuildCronJob_RespectsSuspended(t *testing.T) {
	in := newInput()
	in.Spec.Suspended = true
	files := build(in)
	cj := findResource(t, files, "cronjob.yaml")
	spec := cj["spec"].(map[string]any)
	if spec["suspend"] != true {
		t.Errorf("expected suspend=true on resulting CronJob")
	}
}

func TestBuildCronJob_AppliesDefaultsViaReadInput(t *testing.T) {
	yamlBody := `
apiVersion: platform.kratix.io/v1alpha1
kind: ScheduledJob
metadata:
  name: minimal
  namespace: default
spec:
  schedule: "*/5 * * * *"
  image: example/job:v1
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
	if in.Spec.TimeoutSeconds == nil || *in.Spec.TimeoutSeconds != defaultTimeout {
		t.Errorf("default timeoutSeconds not applied: %+v", in.Spec.TimeoutSeconds)
	}
	if in.Spec.MaxRetries == nil || *in.Spec.MaxRetries != defaultRetries {
		t.Errorf("default maxRetries not applied: %+v", in.Spec.MaxRetries)
	}
}

func TestBuildCronJob_TimeoutZeroOmitsActiveDeadline(t *testing.T) {
	in := newInput()
	zero := int64(0)
	in.Spec.TimeoutSeconds = &zero
	files := build(in)
	cj := findResource(t, files, "cronjob.yaml")
	spec := cj["spec"].(map[string]any)
	jt := spec["jobTemplate"].(map[string]any)
	js := jt["spec"].(map[string]any)
	if _, ok := js["activeDeadlineSeconds"]; ok {
		t.Errorf("timeout=0 must suppress activeDeadlineSeconds; got %+v", js)
	}
}

func TestBuildAudit_CarriesSpecYAML(t *testing.T) {
	files := build(newInput())
	cm := findResource(t, files, "audit-config.yaml")
	data := cm["data"].(map[string]any)
	body := data["spec.yaml"].(string)
	if !bytes.Contains([]byte(body), []byte("nightly-cleanup")) && !bytes.Contains([]byte(body), []byte("0 3 * * *")) {
		t.Errorf("audit spec.yaml missing recognisable content: %q", body)
	}
}

func TestReadInput_RejectsMissingRequired(t *testing.T) {
	cases := []string{
		``,
		`apiVersion: x
kind: ScheduledJob
metadata:
  name: x`, // missing namespace + spec
		`apiVersion: x
kind: ScheduledJob
metadata:
  name: x
  namespace: y
spec:
  image: i`, // missing schedule
		`apiVersion: x
kind: ScheduledJob
metadata:
  name: x
  namespace: y
spec:
  schedule: '*/5 * * * *'`, // missing image
	}
	for i, body := range cases {
		tmp := t.TempDir()
		path := filepath.Join(tmp, "object.yaml")
		os.WriteFile(path, []byte(body), 0o644)
		if _, err := readInput(path); err == nil {
			t.Errorf("case %d: expected error for body %q", i, body)
		}
	}
}

func TestWriteAll_FilesParseAsYAML(t *testing.T) {
	tmp := t.TempDir()
	if err := writeAll(tmp, build(newInput())); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(tmp, e.Name()))
		var v any
		if err := yaml.Unmarshal(data, &v); err != nil {
			t.Errorf("%s: not valid yaml: %v", e.Name(), err)
		}
	}
}

func findResource(t *testing.T, files []fileOut, name string) map[string]any {
	t.Helper()
	for _, f := range files {
		if f.name == name {
			return f.body
		}
	}
	t.Fatalf("file %q not in output", name)
	return nil
}
