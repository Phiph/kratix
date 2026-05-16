// Command pipeline is the Kratix configure pipeline for the
// health-summary-agent Promise.
//
// It runs once per HealthSummaryAgent reconcile. Inputs:
//
//   - /kratix/input/object.yaml  — the HealthSummaryAgent CR
//
// Outputs (the runtime manifests Kratix will apply):
//
//   - /kratix/output/deployment.yaml         — the agent Deployment
//   - /kratix/output/service.yaml            — ClusterIP for the forwarder
//   - /kratix/output/configmap.yaml          — digestSchedule + subscribe
//   - /kratix/output/serviceaccount.yaml     — agent SA
//   - /kratix/output/rbac.yaml               — Role + RoleBinding (events:create)
//   - /kratix/output/cloudeventsink.yaml     — subscription routed to the Service
//
// The pipeline is intentionally a single binary with no Kubernetes client
// imports: it reads YAML in, writes YAML out, and lets Kratix's standard
// work-creator handle the apply.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// hsaInput is the slice of HealthSummaryAgent we care about. Unknown fields
// are ignored — this is forward-compatible with future CRD additions.
type hsaInput struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		DigestSchedule string   `yaml:"digestSchedule"`
		Subscribe      []string `yaml:"subscribe"`
		Image          string   `yaml:"image"`
	} `yaml:"spec"`
}

const (
	defaultInputPath  = "/kratix/input/object.yaml"
	defaultOutputDir  = "/kratix/output"
	defaultSchedule   = "@hourly"
	defaultImage      = "ghcr.io/syntasso/health-summary-agent:v0.1"
	servicePort       = 8080
	receiverPathLocal = "/events"
)

func main() {
	inputPath := envOr("KRATIX_INPUT_OBJECT", defaultInputPath)
	outputDir := envOr("KRATIX_OUTPUT_DIR", defaultOutputDir)

	in, err := readInput(inputPath)
	if err != nil {
		die("read input: %v", err)
	}

	resources := build(in)
	if err := writeAll(outputDir, resources); err != nil {
		die("write output: %v", err)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "health-summary-agent-pipeline: "+format+"\n", args...)
	os.Exit(1)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func readInput(path string) (hsaInput, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return hsaInput{}, err
	}
	var in hsaInput
	if err := yaml.Unmarshal(data, &in); err != nil {
		return hsaInput{}, err
	}
	if in.Metadata.Name == "" {
		return hsaInput{}, fmt.Errorf("input %s missing metadata.name", path)
	}
	if in.Metadata.Namespace == "" {
		// Resource Requests are namespaced; missing namespace is a Kratix
		// invariant breach, not a user error. Fail loud.
		return hsaInput{}, fmt.Errorf("input %s missing metadata.namespace", path)
	}
	if in.Spec.DigestSchedule == "" {
		in.Spec.DigestSchedule = defaultSchedule
	}
	if len(in.Spec.Subscribe) == 0 {
		in.Spec.Subscribe = []string{"kratix.*"}
	}
	if in.Spec.Image == "" {
		in.Spec.Image = defaultImage
	}
	return in, nil
}

// resourceFile holds the on-disk name + parsed YAML body for one output
// resource. Keeping them as map[string]any keeps the pipeline free of any
// k8s.io/api dependency — we never need to typecheck these.
type resourceFile struct {
	filename string
	body     map[string]any
}

func build(in hsaInput) []resourceFile {
	name := "health-summary-agent-" + in.Metadata.Name
	ns := in.Metadata.Namespace

	commonLabels := map[string]any{
		"app.kubernetes.io/name":      "health-summary-agent",
		"app.kubernetes.io/instance":  in.Metadata.Name,
		"app.kubernetes.io/component": "eventing-agent",
	}

	return []resourceFile{
		{"configmap.yaml", buildConfigMap(name, ns, commonLabels, in)},
		{"serviceaccount.yaml", buildServiceAccount(name, ns, commonLabels)},
		{"rbac.yaml", buildRBAC(name, ns, commonLabels)},
		{"deployment.yaml", buildDeployment(name, ns, commonLabels, in)},
		{"service.yaml", buildService(name, ns, commonLabels)},
		{"cloudeventsink.yaml", buildSink(name, ns, commonLabels, in)},
	}
}

func buildConfigMap(name, ns string, labels map[string]any, in hsaInput) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    labels,
		},
		"data": map[string]any{
			"digestSchedule": in.Spec.DigestSchedule,
			"subscribe":      strings.Join(in.Spec.Subscribe, ","),
		},
	}
}

func buildServiceAccount(name, ns string, labels map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    labels,
		},
	}
}

func buildRBAC(name, ns string, labels map[string]any) map[string]any {
	// Two RBAC objects in one file: Role + RoleBinding. We emit them as a
	// list under a single "kratix-list" wrapper so multi-document YAML
	// works without juggling separator tokens.
	role := map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "Role",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    labels,
		},
		"rules": []any{
			// The agent re-emits its own CloudEvents via kratix-emit (or
			// directly creating Events). Scope: this namespace only.
			map[string]any{
				"apiGroups": []any{""},
				"resources": []any{"events"},
				"verbs":     []any{"create", "patch"},
			},
		},
	}
	binding := map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "RoleBinding",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    labels,
		},
		"roleRef": map[string]any{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "Role",
			"name":     name,
		},
		"subjects": []any{
			map[string]any{
				"kind":      "ServiceAccount",
				"name":      name,
				"namespace": ns,
			},
		},
	}
	// The writer encodes one resourceFile as multi-doc YAML when "items" is
	// present and "kind"=="List".
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "List",
		"items":      []any{role, binding},
	}
}

func buildDeployment(name, ns string, labels map[string]any, in hsaInput) map[string]any {
	selectorLabels := map[string]any{
		"app.kubernetes.io/name":     "health-summary-agent",
		"app.kubernetes.io/instance": in.Metadata.Name,
	}
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    labels,
		},
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{
				"matchLabels": selectorLabels,
			},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": labels,
				},
				"spec": map[string]any{
					"serviceAccountName": name,
					"securityContext": map[string]any{
						"runAsNonRoot": true,
						"seccompProfile": map[string]any{
							"type": "RuntimeDefault",
						},
					},
					"containers": []any{
						map[string]any{
							"name":  "agent",
							"image": in.Spec.Image,
							"ports": []any{
								map[string]any{
									"name":          "http",
									"containerPort": servicePort,
								},
							},
							"env": []any{
								map[string]any{
									"name":  "AGENT_INSTANCE",
									"value": in.Metadata.Name,
								},
								map[string]any{
									"name":  "AGENT_NAMESPACE",
									"value": ns,
								},
								map[string]any{
									"name": "POD_NAMESPACE",
									"valueFrom": map[string]any{
										"fieldRef": map[string]any{
											"fieldPath": "metadata.namespace",
										},
									},
								},
							},
							"envFrom": []any{
								map[string]any{
									"configMapRef": map[string]any{
										"name": name,
									},
								},
							},
							"readinessProbe": map[string]any{
								"httpGet": map[string]any{
									"path": "/healthz",
									"port": "http",
								},
							},
							"securityContext": map[string]any{
								"allowPrivilegeEscalation": false,
								"capabilities": map[string]any{
									"drop": []any{"ALL"},
								},
								"readOnlyRootFilesystem": true,
							},
							"resources": map[string]any{
								"requests": map[string]any{
									"cpu":    "25m",
									"memory": "64Mi",
								},
								"limits": map[string]any{
									"cpu":    "200m",
									"memory": "256Mi",
								},
							},
						},
					},
				},
			},
		},
	}
}

func buildService(name, ns string, labels map[string]any) map[string]any {
	selectorLabels := map[string]any{
		"app.kubernetes.io/name":     "health-summary-agent",
		"app.kubernetes.io/instance": strings.TrimPrefix(name, "health-summary-agent-"),
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    labels,
		},
		"spec": map[string]any{
			"type":     "ClusterIP",
			"selector": selectorLabels,
			"ports": []any{
				map[string]any{
					"name":       "http",
					"port":       servicePort,
					"targetPort": "http",
				},
			},
		},
	}
}

func buildSink(name, ns string, labels map[string]any, in hsaInput) map[string]any {
	// CloudEventSink is cluster-scoped (see eventing/api/v1alpha1) — names
	// must be unique cluster-wide. Disambiguate with the instance namespace.
	sinkName := fmt.Sprintf("%s-%s", name, ns)
	url := fmt.Sprintf("http://%s.%s.svc:%d%s", name, ns, servicePort, receiverPathLocal)
	return map[string]any{
		"apiVersion": "eventing.kratix.io/v1alpha1",
		"kind":       "CloudEventSink",
		"metadata": map[string]any{
			"name":   sinkName,
			"labels": labels,
		},
		"spec": map[string]any{
			"url":        url,
			"typeFilter": toAnySlice(in.Spec.Subscribe),
		},
	}
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func writeAll(dir string, files []resourceFile) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		path := filepath.Join(dir, f.filename)
		if err := writeYAML(path, f.body); err != nil {
			return fmt.Errorf("%s: %w", f.filename, err)
		}
	}
	return nil
}

// writeYAML serialises body as YAML. If body is a List (apiVersion=v1,
// kind=List), it writes a multi-document file with --- separators instead
// — kubectl, kustomize, and Kratix's work-creator all handle either form.
func writeYAML(path string, body map[string]any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if isList(body) {
		items, _ := body["items"].([]any)
		enc := yaml.NewEncoder(f)
		enc.SetIndent(2)
		for _, item := range items {
			if err := enc.Encode(item); err != nil {
				return err
			}
		}
		return enc.Close()
	}

	enc := yaml.NewEncoder(f)
	enc.SetIndent(2)
	defer enc.Close()
	return enc.Encode(body)
}

func isList(body map[string]any) bool {
	if av, _ := body["apiVersion"].(string); av != "v1" {
		return false
	}
	if k, _ := body["kind"].(string); k != "List" {
		return false
	}
	_, ok := body["items"].([]any)
	return ok
}
