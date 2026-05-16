package main

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	eventingv1alpha1 "github.com/syntasso/kratix/eventing/api/v1alpha1"
)

func newBundle() *eventingv1alpha1.PromiseBundle {
	return &eventingv1alpha1.PromiseBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: "scheduled-job",
			UID:  types.UID("pbun-uid"),
		},
		Spec: eventingv1alpha1.PromiseBundleSpec{
			PromiseRef: eventingv1alpha1.PromiseBundlePromiseRef{Name: "scheduled-job"},
		},
	}
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(sch))
	utilruntime.Must(eventingv1alpha1.AddToScheme(sch))
	return fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(objs...).
		Build()
}

func TestDecodeCompanion_InlineHappy(t *testing.T) {
	inline := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x","namespace":"y"},"data":{"k":"v"}}`
	c := eventingv1alpha1.Companion{
		Name:   "inline-cm",
		Inline: &runtime.RawExtension{Raw: []byte(inline)},
	}
	u, err := decodeCompanion(context.Background(), newFakeClient(t), newBundle(), c)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if u.GetKind() != "ConfigMap" || u.GetAPIVersion() != "v1" {
		t.Errorf("decoded kind/apiVersion = %s/%s", u.GetKind(), u.GetAPIVersion())
	}
	owners := u.GetOwnerReferences()
	if len(owners) != 1 || owners[0].Kind != "PromiseBundle" || owners[0].UID != types.UID("pbun-uid") {
		t.Errorf("owner refs = %+v", owners)
	}
}

func TestDecodeCompanion_RefFromConfigMap(t *testing.T) {
	manifest := `apiVersion: v1
kind: ConfigMap
metadata:
  name: derived
  namespace: y
data:
  hello: world`
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "bundle-source", Namespace: "kratix-platform-system"},
		Data:       map[string]string{"manifest.yaml": manifest},
	}
	cli := newFakeClient(t, cm)
	c := eventingv1alpha1.Companion{
		Name: "ref-cm",
		Ref: &eventingv1alpha1.CompanionRef{
			ConfigMap: eventingv1alpha1.CompanionConfigMapRef{
				Name:      "bundle-source",
				Namespace: "kratix-platform-system",
				Key:       "manifest.yaml",
			},
		},
	}
	u, err := decodeCompanion(context.Background(), cli, newBundle(), c)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if u.GetName() != "derived" {
		t.Errorf("name = %q", u.GetName())
	}
}

func TestDecodeCompanion_BothInlineAndRefRejected(t *testing.T) {
	c := eventingv1alpha1.Companion{
		Name:   "bad",
		Inline: &runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap"}`)},
		Ref:    &eventingv1alpha1.CompanionRef{ConfigMap: eventingv1alpha1.CompanionConfigMapRef{Name: "x", Namespace: "y", Key: "z"}},
	}
	_, err := decodeCompanion(context.Background(), newFakeClient(t), newBundle(), c)
	if err == nil || !isDecodeError(err) {
		t.Fatalf("expected decode-error, got %v", err)
	}
}

func TestDecodeCompanion_NeitherRejected(t *testing.T) {
	c := eventingv1alpha1.Companion{Name: "empty"}
	_, err := decodeCompanion(context.Background(), newFakeClient(t), newBundle(), c)
	if err == nil || !isDecodeError(err) {
		t.Fatalf("expected decode-error, got %v", err)
	}
}

func TestDecodeCompanion_MultiDocRejected(t *testing.T) {
	multidoc := `apiVersion: v1
kind: ConfigMap
metadata:
  name: one
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: two`
	c := eventingv1alpha1.Companion{
		Name:   "multi",
		Inline: &runtime.RawExtension{Raw: []byte(multidoc)},
	}
	_, err := decodeCompanion(context.Background(), newFakeClient(t), newBundle(), c)
	if err == nil {
		t.Fatal("expected error for multi-doc YAML")
	}
	if !isDecodeError(err) || !strings.Contains(err.Error(), "multi-document") {
		t.Errorf("expected decode-error mentioning 'multi-document', got %v", err)
	}
}

func TestDecodeCompanion_RefConfigMapNotFound(t *testing.T) {
	cli := newFakeClient(t)
	c := eventingv1alpha1.Companion{
		Name: "missing-cm",
		Ref: &eventingv1alpha1.CompanionRef{
			ConfigMap: eventingv1alpha1.CompanionConfigMapRef{Name: "nope", Namespace: "ns", Key: "x"},
		},
	}
	_, err := decodeCompanion(context.Background(), cli, newBundle(), c)
	if err == nil || !isDecodeError(err) {
		t.Fatalf("expected decode-error for missing ConfigMap, got %v", err)
	}
}

func TestSetOwner_ReplacesPriorPromiseBundleOwner(t *testing.T) {
	// If a companion was previously owned by a different PromiseBundle, the
	// new owner should replace, not duplicate. Non-PromiseBundle owners
	// (e.g. a Deployment owning a Pod) should be left untouched.
	c := eventingv1alpha1.Companion{
		Name:   "x",
		Inline: &runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x","namespace":"y","ownerReferences":[{"apiVersion":"eventing.kratix.io/v1alpha1","kind":"PromiseBundle","name":"old","uid":"old-uid","controller":true,"blockOwnerDeletion":true},{"apiVersion":"apps/v1","kind":"Deployment","name":"keep-me","uid":"keep-uid"}]}}`)},
	}
	u, err := decodeCompanion(context.Background(), newFakeClient(t), newBundle(), c)
	if err != nil {
		t.Fatal(err)
	}
	owners := u.GetOwnerReferences()
	var bundleCount, deploymentCount int
	for _, o := range owners {
		switch o.Kind {
		case "PromiseBundle":
			bundleCount++
			if o.Name == "old" {
				t.Errorf("stale PromiseBundle owner not replaced: %+v", o)
			}
		case "Deployment":
			deploymentCount++
		}
	}
	if bundleCount != 1 {
		t.Errorf("expected exactly one PromiseBundle owner ref, got %d", bundleCount)
	}
	if deploymentCount != 1 {
		t.Errorf("non-bundle owner refs should be preserved, got %d", deploymentCount)
	}
}
