package v1alpha1

import "github.com/syntasso/kratix/lib/fetchers"

// +kubebuilder:object:generate=false
//
//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 . PromiseFetcher
type PromiseFetcher interface {
	FromURL(params fetchers.FromURLParams) (*Promise, error)
}
