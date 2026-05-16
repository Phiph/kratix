//go:build envtest

// Real-apiserver smoke test for the bundle controller.
//
// Run with:
//
//	make test-envtest
//
// Asserts the full reconcile: PromiseBundle + Promise (made Available) →
// companions applied with the bundle as owner.
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kratixv1alpha1 "github.com/syntasso/kratix/api/v1alpha1"
	eventingv1alpha1 "github.com/syntasso/kratix/eventing/api/v1alpha1"
)

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func loadCRDs(t *testing.T, paths ...string) []*apiextv1.CustomResourceDefinition {
	t.Helper()
	var out []*apiextv1.CustomResourceDefinition
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)
		for {
			crd := &apiextv1.CustomResourceDefinition{}
			if err := dec.Decode(crd); err != nil {
				break
			}
			if crd.Kind == "CustomResourceDefinition" {
				out = append(out, crd)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no CRDs decoded from %v", paths)
	}
	return out
}

func TestBundle_Envtest_PromiseAvailable_AppliesCompanions(t *testing.T) {
	root := findRepoRoot(t)
	crds := loadCRDs(t,
		filepath.Join(root, "eventing", "config", "crd", "promisebundle.yaml"),
		filepath.Join(root, "config", "crd", "bases", "platform.kratix.io_promises.yaml"),
	)
	env := &envtest.Environment{CRDs: crds}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	sch := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(sch))
	utilruntime.Must(eventingv1alpha1.AddToScheme(sch))
	utilruntime.Must(kratixv1alpha1.AddToScheme(sch))
	cli, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	// 1. Create a minimal Promise (we only need .status.status).
	promise := &kratixv1alpha1.Promise{
		ObjectMeta: metav1.ObjectMeta{Name: "scheduled-job"},
	}
	if err := cli.Create(context.Background(), promise); err != nil {
		t.Fatalf("create promise: %v", err)
	}
	// Force-set Promise status to Available via the status subresource.
	promise.Status.Status = kratixv1alpha1.PromiseStatusAvailable
	if err := cli.Status().Update(context.Background(), promise); err != nil {
		t.Fatalf("status update: %v", err)
	}

	// 2. Apply a bundle that ships one companion: a ConfigMap. Simple
	//    enough to compile without producing other CRDs; rich enough to
	//    prove apply + owner-reference.
	bundle := &eventingv1alpha1.PromiseBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "scheduled-job"},
		Spec: eventingv1alpha1.PromiseBundleSpec{
			PromiseRef: eventingv1alpha1.PromiseBundlePromiseRef{Name: "scheduled-job"},
			Companions: []eventingv1alpha1.Companion{
				{
					Name: "test-config",
					Inline: &runtime.RawExtension{
						Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"envtest-cm","namespace":"default"},"data":{"hello":"world"}}`),
					},
				},
			},
		},
	}
	if err := cli.Create(context.Background(), bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	// Fetch back to get UID populated for owner-reference assertion.
	if err := cli.Get(context.Background(), client.ObjectKey{Name: "scheduled-job"}, bundle); err != nil {
		t.Fatal(err)
	}

	// 3. Reconcile once.
	r := &Reconciler{Client: cli, FieldManager: "bundle-envtest", Now: time.Now}
	_, err = r.Reconcile(context.Background(), reqFor("scheduled-job"))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// 4. The ConfigMap should now exist, owned by the bundle.
	var cm corev1.ConfigMap
	if err := cli.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "envtest-cm"}, &cm); err != nil {
		t.Fatalf("companion ConfigMap missing: %v", err)
	}
	if cm.Data["hello"] != "world" {
		t.Errorf("ConfigMap data not applied: %+v", cm.Data)
	}
	var foundOwner bool
	for _, o := range cm.OwnerReferences {
		if o.Kind == "PromiseBundle" && o.Name == "scheduled-job" && o.UID == bundle.UID {
			foundOwner = true
		}
	}
	if !foundOwner {
		t.Errorf("expected PromiseBundle owner reference; got %+v", cm.OwnerReferences)
	}

	// 5. Bundle status should reflect a successful apply.
	if err := cli.Get(context.Background(), client.ObjectKey{Name: "scheduled-job"}, bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Status.Companions) != 1 {
		t.Fatalf("expected 1 companion status, got %d", len(bundle.Status.Companions))
	}
	if bundle.Status.Companions[0].Result != eventingv1alpha1.CompanionApplied {
		t.Errorf("companion result = %q", bundle.Status.Companions[0].Result)
	}
	var readyTrue bool
	for _, c := range bundle.Status.Conditions {
		if c.Type == "Ready" && c.Status == metav1.ConditionTrue {
			readyTrue = true
		}
	}
	if !readyTrue {
		t.Errorf("expected Ready=True condition; conditions = %+v", bundle.Status.Conditions)
	}
}

func TestBundle_Envtest_PromiseUnavailable_BlocksApply(t *testing.T) {
	root := findRepoRoot(t)
	crds := loadCRDs(t,
		filepath.Join(root, "eventing", "config", "crd", "promisebundle.yaml"),
		filepath.Join(root, "config", "crd", "bases", "platform.kratix.io_promises.yaml"),
	)
	env := &envtest.Environment{CRDs: crds}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	sch := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(sch))
	utilruntime.Must(eventingv1alpha1.AddToScheme(sch))
	utilruntime.Must(kratixv1alpha1.AddToScheme(sch))
	cli, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatal(err)
	}

	// Promise exists but is not Available.
	promise := &kratixv1alpha1.Promise{ObjectMeta: metav1.ObjectMeta{Name: "scheduled-job"}}
	if err := cli.Create(context.Background(), promise); err != nil {
		t.Fatal(err)
	}
	// Leave .status.status empty.

	bundle := &eventingv1alpha1.PromiseBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "scheduled-job"},
		Spec: eventingv1alpha1.PromiseBundleSpec{
			PromiseRef: eventingv1alpha1.PromiseBundlePromiseRef{Name: "scheduled-job"},
			Companions: []eventingv1alpha1.Companion{{
				Name:   "blocked",
				Inline: &runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"should-not-exist","namespace":"default"}}`)},
			}},
		},
	}
	if err := cli.Create(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}

	r := &Reconciler{Client: cli, FieldManager: "bundle-envtest", Now: time.Now}
	if _, err := r.Reconcile(context.Background(), reqFor("scheduled-job")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Companion must NOT have been applied.
	var cm corev1.ConfigMap
	err = cli.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "should-not-exist"}, &cm)
	if !apierrors.IsNotFound(err) {
		t.Errorf("companion was applied despite Promise not being Available: %v", err)
	}

	// Status should reflect the block.
	if err := cli.Get(context.Background(), client.ObjectKey{Name: "scheduled-job"}, bundle); err != nil {
		t.Fatal(err)
	}
	var blocked bool
	for _, c := range bundle.Status.Conditions {
		if c.Type == "Ready" && c.Reason == reasonPromiseNotReady {
			blocked = true
		}
	}
	if !blocked {
		t.Errorf("expected Ready=False/PromiseNotReady; conditions = %+v", bundle.Status.Conditions)
	}
}

func reqFor(name string) ctrl.Request {
	return reconcile.Request{NamespacedName: k8stypes.NamespacedName{Name: name}}
}
