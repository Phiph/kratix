// Command pipeline is the ScheduledJob Promise's configure pipeline.
//
// Runs once per ScheduledJob reconcile. Inputs:
//
//	/kratix/input/object.yaml   — the ScheduledJob RR
//
// Outputs:
//
//	/kratix/output/cronjob.yaml     — managed CronJob
//	/kratix/output/audit-config.yaml — audit ConfigMap (immutable record of the spec)
//
// The pipeline is intentionally a single binary with no K8s client
// imports. It reads YAML in, writes YAML out, and lets Kratix's
// work-creator handle the apply.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type scheduledJobInput struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Schedule       string   `yaml:"schedule"`
		Image          string   `yaml:"image"`
		Command        []string `yaml:"command"`
		Args           []string `yaml:"args"`
		TimeoutSeconds *int64   `yaml:"timeoutSeconds"`
		MaxRetries     *int32   `yaml:"maxRetries"`
		Suspended      bool     `yaml:"suspended"`
	} `yaml:"spec"`
}

const (
	defaultInputPath = "/kratix/input/object.yaml"
	defaultOutputDir = "/kratix/output"
	defaultTimeout   = int64(600)
	defaultRetries   = int32(2)
)

func main() {
	inputPath := envOr("KRATIX_INPUT_OBJECT", defaultInputPath)
	outputDir := envOr("KRATIX_OUTPUT_DIR", defaultOutputDir)
	in, err := readInput(inputPath)
	if err != nil {
		die("read input: %v", err)
	}
	files := build(in)
	if err := writeAll(outputDir, files); err != nil {
		die("write output: %v", err)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "scheduled-job-pipeline: "+format+"\n", args...)
	os.Exit(1)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func readInput(path string) (scheduledJobInput, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return scheduledJobInput{}, err
	}
	var in scheduledJobInput
	if err := yaml.Unmarshal(data, &in); err != nil {
		return scheduledJobInput{}, err
	}
	if in.Metadata.Name == "" {
		return scheduledJobInput{}, fmt.Errorf("missing metadata.name")
	}
	if in.Metadata.Namespace == "" {
		return scheduledJobInput{}, fmt.Errorf("missing metadata.namespace")
	}
	if in.Spec.Schedule == "" {
		return scheduledJobInput{}, fmt.Errorf("spec.schedule is required")
	}
	if in.Spec.Image == "" {
		return scheduledJobInput{}, fmt.Errorf("spec.image is required")
	}
	if in.Spec.TimeoutSeconds == nil {
		t := defaultTimeout
		in.Spec.TimeoutSeconds = &t
	}
	if in.Spec.MaxRetries == nil {
		r := defaultRetries
		in.Spec.MaxRetries = &r
	}
	return in, nil
}

type fileOut struct {
	name string
	body map[string]any
}

func build(in scheduledJobInput) []fileOut {
	name := "scheduled-job-" + in.Metadata.Name
	ns := in.Metadata.Namespace
	labels := map[string]any{
		"app.kubernetes.io/name":      "scheduled-job",
		"app.kubernetes.io/instance":  in.Metadata.Name,
		"app.kubernetes.io/component": "managed-cronjob",
		"platform.kratix.io/owned-by": "scheduled-job-promise",
	}
	return []fileOut{
		{"cronjob.yaml", buildCronJob(name, ns, labels, in)},
		{"audit-config.yaml", buildAuditConfigMap(name, ns, labels, in)},
	}
}

func buildCronJob(name, ns string, labels map[string]any, in scheduledJobInput) map[string]any {
	containers := []any{
		map[string]any{
			"name":    "job",
			"image":   in.Spec.Image,
			"command": stringSliceToAny(in.Spec.Command),
			"args":    stringSliceToAny(in.Spec.Args),
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false,
				"runAsNonRoot":             true,
				"capabilities": map[string]any{"drop": []any{"ALL"}},
			},
		},
	}

	jobSpec := map[string]any{
		"backoffLimit": int64(*in.Spec.MaxRetries),
		"template": map[string]any{
			"metadata": map[string]any{"labels": labels},
			"spec": map[string]any{
				"restartPolicy": "OnFailure",
				"containers":    containers,
			},
		},
	}
	if *in.Spec.TimeoutSeconds > 0 {
		jobSpec["activeDeadlineSeconds"] = *in.Spec.TimeoutSeconds
	}

	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "CronJob",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    labels,
		},
		"spec": map[string]any{
			"schedule":                   in.Spec.Schedule,
			"suspend":                    in.Spec.Suspended,
			"successfulJobsHistoryLimit": 3,
			"failedJobsHistoryLimit":     3,
			"concurrencyPolicy":          "Forbid",
			"jobTemplate": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     jobSpec,
			},
		},
	}
}

// buildAuditConfigMap captures the spec snapshot at apply time. The agents
// (and humans) read this when forming or evaluating a remediation proposal —
// the rationale "this schedule has changed 5 times this week" needs the
// audit ConfigMap's history to be true. The pipeline only writes the
// *current* snapshot; downstream tooling appends a history file if needed.
func buildAuditConfigMap(name, ns string, labels map[string]any, in scheduledJobInput) map[string]any {
	data, _ := yaml.Marshal(in.Spec)
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name + "-audit",
			"namespace": ns,
			"labels":    labels,
		},
		"data": map[string]any{
			"spec.yaml": string(data),
		},
	}
}

func stringSliceToAny(in []string) []any {
	if len(in) == 0 {
		return nil
	}
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func writeAll(dir string, files []fileOut) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		fh, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
		enc := yaml.NewEncoder(fh)
		enc.SetIndent(2)
		if err := enc.Encode(f.body); err != nil {
			fh.Close()
			return fmt.Errorf("%s: %w", f.name, err)
		}
		enc.Close()
		fh.Close()
	}
	return nil
}
