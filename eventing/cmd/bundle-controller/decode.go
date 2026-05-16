package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	eventingv1alpha1 "github.com/syntasso/kratix/eventing/api/v1alpha1"
)

// errDecode signals input we deliberately reject (returned to caller as a
// terminal status; the controller does not retry the reconcile loop on
// this error class).
type errDecode struct{ msg string }

func (e errDecode) Error() string { return e.msg }

func decodeFailure(format string, args ...any) error {
	return errDecode{msg: fmt.Sprintf(format, args...)}
}

func isDecodeError(err error) bool {
	var e errDecode
	return errors.As(err, &e)
}

// decodeCompanion resolves one Companion into a fully-typed
// unstructured.Unstructured ready for server-side-apply. For ref types it
// fetches the backing ConfigMap. The owner reference to the PromiseBundle
// is set here so callers don't have to remember.
//
// Returns errDecode for invalid input; any other error is transient (the
// caller may retry the whole reconcile).
func decodeCompanion(
	ctx context.Context,
	cli client.Client,
	bundle *eventingv1alpha1.PromiseBundle,
	c eventingv1alpha1.Companion,
) (*unstructured.Unstructured, error) {
	switch {
	case c.Inline != nil && c.Ref != nil:
		return nil, decodeFailure("companion %q: inline and ref are mutually exclusive", c.Name)
	case c.Inline == nil && c.Ref == nil:
		return nil, decodeFailure("companion %q: exactly one of inline or ref must be set", c.Name)
	case c.Inline != nil:
		return decodeInline(c.Name, c.Inline.Raw, bundle)
	default:
		return decodeFromConfigMap(ctx, cli, c, bundle)
	}
}

func decodeInline(name string, raw []byte, bundle *eventingv1alpha1.PromiseBundle) (*unstructured.Unstructured, error) {
	if len(raw) == 0 {
		return nil, decodeFailure("companion %q: inline body is empty", name)
	}
	u, err := decodeYAMLToUnstructured(raw)
	if err != nil {
		return nil, decodeFailure("companion %q: %v", name, err)
	}
	if u.GetKind() == "" || u.GetAPIVersion() == "" {
		return nil, decodeFailure("companion %q: decoded object has empty kind/apiVersion", name)
	}
	setOwner(u, bundle)
	return u, nil
}

func decodeFromConfigMap(
	ctx context.Context,
	cli client.Client,
	c eventingv1alpha1.Companion,
	bundle *eventingv1alpha1.PromiseBundle,
) (*unstructured.Unstructured, error) {
	ref := c.Ref.ConfigMap
	var cm corev1.ConfigMap
	if err := cli.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, decodeFailure("companion %q: ConfigMap %s/%s not found", c.Name, ref.Namespace, ref.Name)
		}
		return nil, fmt.Errorf("get ConfigMap %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	body, ok := cm.Data[ref.Key]
	if !ok {
		return nil, decodeFailure("companion %q: key %q missing in ConfigMap %s/%s", c.Name, ref.Key, ref.Namespace, ref.Name)
	}
	u, err := decodeYAMLToUnstructured([]byte(body))
	if err != nil {
		return nil, decodeFailure("companion %q: %v", c.Name, err)
	}
	if u.GetKind() == "" || u.GetAPIVersion() == "" {
		return nil, decodeFailure("companion %q: decoded object has empty kind/apiVersion", c.Name)
	}
	setOwner(u, bundle)
	return u, nil
}

// decodeYAMLToUnstructured parses a single YAML document into an
// unstructured.Unstructured. Multi-doc input is rejected — a Companion is
// one resource. Producers that need to ship multiple resources together
// add multiple Companion entries.
func decodeYAMLToUnstructured(body []byte) (*unstructured.Unstructured, error) {
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(body), 4096)
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("decode yaml: %w", err)
	}
	// Reject second document if present.
	var more map[string]any
	if err := dec.Decode(&more); err == nil && more != nil {
		return nil, fmt.Errorf("multi-document YAML is not allowed in a single companion; split into multiple companions")
	}
	return &unstructured.Unstructured{Object: obj}, nil
}

// setOwner makes the PromiseBundle the controller-owner of u so that
// deleting the bundle cascades to its companions via standard garbage
// collection.
func setOwner(u *unstructured.Unstructured, bundle *eventingv1alpha1.PromiseBundle) {
	truthy := true
	owner := metav1.OwnerReference{
		APIVersion:         eventingv1alpha1.GroupVersion.String(),
		Kind:               "PromiseBundle",
		Name:               bundle.GetName(),
		UID:                bundle.GetUID(),
		Controller:         &truthy,
		BlockOwnerDeletion: &truthy,
	}
	// Cluster-scoped resources can reference a cluster-scoped owner.
	// Namespaced resources cannot reference an owner in a different
	// namespace — but a cluster-scoped PromiseBundle works for both
	// because cluster-scoped owners are always allowed.
	existing := u.GetOwnerReferences()
	// Replace any prior PromiseBundle owner reference; leave other
	// owners untouched.
	out := existing[:0]
	for _, or := range existing {
		if or.Kind == "PromiseBundle" && or.APIVersion == owner.APIVersion {
			continue
		}
		out = append(out, or)
	}
	out = append(out, owner)
	u.SetOwnerReferences(out)
}

