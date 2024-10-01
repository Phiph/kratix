package fetchers

import (
	"context"
	"fmt"
	"net/http"

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
)

type URLFetcher struct {
	Client client.Client
	ctx    context.Context
	logger logr.Logger
}

type opts struct {
	ctx    context.Context
	client client.Client
	logger logr.Logger
}
type FromURLParams struct {
	URL    string
	Secret *v1.SecretReference
}

func (u *URLFetcher) FromURL(params FromURLParams) (*v1alpha1.Promise, error) {
	opts := opts{
		client: u.Client,
		ctx:    u.ctx,
		logger: u.logger,
	}

	client := &http.Client{}
	req, err := http.NewRequest("GET", params.URL, nil)
	if err != nil {
		return nil, err
	}

	if params.Secret != nil {
		secretRef := types.NamespacedName{
			Name:      params.Secret.Name,
			Namespace: params.Secret.Namespace,
		}
		secret, err := fetchSecret(opts, secretRef)
		if err != nil {
			return nil, err
		}
		if password, ok := secret.Data["password"]; ok {
			req.Header.Add("Authorization", "Bearer "+string(password))
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get url: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get Promise from URL: status code %d", resp.StatusCode)
	}

	decoder := yaml.NewYAMLOrJSONDecoder(resp.Body, 2048)

	// Use an unstructured k8s object to perform basic validation that the YAML is a k8s
	// object.
	unstructuredPromise := unstructured.Unstructured{}

	var promise v1alpha1.Promise

	if err := decoder.Decode(&unstructuredPromise); err != nil {
		return nil, fmt.Errorf("failed to unmarshal into Kubernetes object: %s", err.Error())
	}

	if kind := unstructuredPromise.GetKind(); kind != "Promise" {
		return nil, fmt.Errorf("expected single Promise object but found object of kind: %s", kind)
	}

	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredPromise.Object, &promise)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal into Promise: %w", err)
	}

	// Attempt to decode again to check if there are multiple objects.
	if err = decoder.Decode(&unstructuredPromise); err == nil {
		return nil, fmt.Errorf("expected single document yaml, but found multiple documents")
	}

	return &promise, nil
}

func fetchSecret(o opts, secretref client.ObjectKey) (*v1.Secret, error) {

	secret := &v1.Secret{}
	secretRef := types.NamespacedName{
		Name:      secretref.Name,
		Namespace: secretref.Namespace,
	}

	if err := o.client.Get(o.ctx, secretRef, secret); err != nil {
		o.logger.Error(err, "unable to fetch resource", "secretRef", secretRef)
		return nil, err
	}

	return secret, nil
}
