package main

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kratixv1alpha1 "github.com/syntasso/kratix/api/v1alpha1"
	eventingv1alpha1 "github.com/syntasso/kratix/eventing/api/v1alpha1"
)

// handlerForPromiseEnqueueBundle returns a handler that, on each
// Promise change, enqueues every PromiseBundle whose spec.promiseRef
// matches the changed Promise's name. This is how the controller learns
// "the Promise just became Available; reconcile any bundles waiting on it"
// without polling.
func handlerForPromiseEnqueueBundle(mgr manager.Manager) handler.EventHandler {
	cli := mgr.GetClient()
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		p, ok := obj.(*kratixv1alpha1.Promise)
		if !ok {
			return nil
		}
		var bundles eventingv1alpha1.PromiseBundleList
		if err := cli.List(ctx, &bundles); err != nil {
			return nil
		}
		var out []reconcile.Request
		for _, b := range bundles.Items {
			if b.Spec.PromiseRef.Name == p.Name {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: b.Name}})
			}
		}
		return out
	})
}
