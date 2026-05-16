package main

import (
	"testing"

	"github.com/syntasso/kratix/eventing/pkg/schema"
)

func TestReadParentFromEnv(t *testing.T) {
	t.Run("namespaced resource", func(t *testing.T) {
		env := map[string]string{
			"KRATIX_OBJECT_KIND":      "Redis",
			"KRATIX_OBJECT_GROUP":     "example.kratix.io",
			"KRATIX_OBJECT_VERSION":   "v1alpha1",
			"KRATIX_OBJECT_NAME":      "my-redis",
			"KRATIX_OBJECT_NAMESPACE": "default",
		}
		pr, err := readParentFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		if pr.APIVersion != "example.kratix.io/v1alpha1" {
			t.Errorf("APIVersion = %q", pr.APIVersion)
		}
		if pr.Namespace != "default" {
			t.Errorf("Namespace = %q", pr.Namespace)
		}
	})

	t.Run("cluster-scoped promise (empty namespace, empty group)", func(t *testing.T) {
		env := map[string]string{
			"KRATIX_OBJECT_KIND":    "Promise",
			"KRATIX_OBJECT_VERSION": "v1alpha1",
			"KRATIX_OBJECT_NAME":    "redis-promise",
		}
		pr, err := readParentFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		if pr.APIVersion != "v1alpha1" {
			t.Errorf("APIVersion = %q (expected version only for empty group)", pr.APIVersion)
		}
		if pr.Namespace != "" {
			t.Errorf("Namespace = %q (expected empty)", pr.Namespace)
		}
	})

	t.Run("missing required env vars", func(t *testing.T) {
		env := map[string]string{"KRATIX_OBJECT_KIND": "Promise"}
		_, err := readParentFromEnv(func(k string) string { return env[k] })
		if err == nil {
			t.Fatal("expected error for missing required env vars")
		}
	})
}

func TestEmissionArgs_Validate(t *testing.T) {
	cases := []struct {
		name       string
		args       emissionArgs
		wantErr    bool
		wantReason string
		wantSev    string
	}{
		{
			name:       "minimal valid",
			args:       emissionArgs{Type: "upstream.fetch.failed"},
			wantReason: "UpstreamFetchFailed",
			wantSev:    schema.SeverityInfo,
		},
		{
			name:       "explicit reason wins",
			args:       emissionArgs{Type: "upstream.fetch.failed", Reason: "CustomReason"},
			wantReason: "CustomReason",
			wantSev:    schema.SeverityInfo,
		},
		{
			name:       "warning severity",
			args:       emissionArgs{Type: "upstream.fetch.failed", Severity: schema.SeverityWarning},
			wantReason: "UpstreamFetchFailed",
			wantSev:    schema.SeverityWarning,
		},
		{
			name:    "missing type",
			args:    emissionArgs{},
			wantErr: true,
		},
		{
			name:    "reserved kratix.* namespace",
			args:    emissionArgs{Type: "kratix.promise.unavailable"},
			wantErr: true,
		},
		{
			name:    "invalid severity",
			args:    emissionArgs{Type: "upstream.fetch.failed", Severity: "critical"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.args.validate()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.args.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", tc.args.Reason, tc.wantReason)
			}
			if tc.args.Severity != tc.wantSev {
				t.Errorf("Severity = %q, want %q", tc.args.Severity, tc.wantSev)
			}
		})
	}
}

func TestReasonFromType(t *testing.T) {
	cases := map[string]string{
		"upstream.fetch.failed":    "UpstreamFetchFailed",
		"single":                   "Single",
		"a.b.c.d":                  "ABCD",
		"trailing.dot.":            "TrailingDot",
		"":                         "",
	}
	for in, want := range cases {
		if got := reasonFromType(in); got != want {
			t.Errorf("reasonFromType(%q) = %q, want %q", in, got, want)
		}
	}
}
