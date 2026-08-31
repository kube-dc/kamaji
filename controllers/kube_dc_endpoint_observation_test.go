// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	"github.com/clastix/kamaji/internal/datastore"
	"github.com/clastix/kamaji/internal/utilities"
)

// checkOnlyConnection is a datastore.Connection whose only behaviour is the
// outcome of Check; every other method panics if reached.
type checkOnlyConnection struct {
	datastore.Connection

	checkErr error
}

func (c checkOnlyConnection) Check(context.Context) error { return c.checkErr }

func TestObserveKubeDCDataStoreEndpoint(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := kamajiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	tcp := &kamajiv1alpha1.TenantControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name: "b1-cp", Namespace: "shalb-dev",
			Annotations: map[string]string{
				utilities.TenantClientEndpointAnnotation:      utilities.TenantClientEndpointClusterIP,
				utilities.TenantClientEndpointTokenAnnotation: "tcp-rv-42",
			},
		},
	}
	ds := &kamajiv1alpha1.DataStore{ObjectMeta: metav1.ObjectMeta{Name: "b1-etcd"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tcp).Build()
	r := &TenantControlPlaneReconciler{Client: cl}

	changed, err := r.observeKubeDCDataStoreEndpoint(context.Background(), tcp, ds)
	if err != nil || !changed {
		t.Fatalf("first observation = changed %t, err %v", changed, err)
	}
	live := &kamajiv1alpha1.TenantControlPlane{}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(tcp), live); err != nil {
		t.Fatal(err)
	}
	if got := live.Annotations[utilities.TenantClientEndpointObservedAnnotation]; got != "" {
		t.Fatalf("TCP reconciler must not claim the soot-owned tenant endpoint observation, got %q", got)
	}
	if got := live.Annotations[utilities.DataStoreEndpointObservedAnnotation]; got != utilities.DataStoreEndpointFingerprint(ds) {
		t.Fatalf("datastore endpoint observation = %q", got)
	}

	changed, err = r.observeKubeDCDataStoreEndpoint(context.Background(), live, ds)
	if err != nil || changed {
		t.Fatalf("idempotent observation = changed %t, err %v", changed, err)
	}

	ds.Spec.ManagementEndpoints = []string{"b1-etcd-lb-ext.shalb-dev.svc:32390"}
	changed, err = r.observeKubeDCDataStoreEndpoint(context.Background(), live, ds)
	if err != nil || !changed {
		t.Fatalf("external datastore observation = changed %t, err %v", changed, err)
	}
}

func TestAckKubeDCDataStoreRoute(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := kamajiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	tcp := &kamajiv1alpha1.TenantControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "b1-cp", Namespace: "shalb-dev"},
	}
	ds := &kamajiv1alpha1.DataStore{ObjectMeta: metav1.ObjectMeta{Name: "b1-etcd"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tcp).Build()
	r := &TenantControlPlaneReconciler{Client: cl}

	// An unreachable route must stop the reconcile with the check error and
	// must NOT acknowledge anything.
	injected := errors.New("dial tcp: connection refused")
	res, err := r.ackKubeDCDataStoreRoute(context.Background(), tcp, ds, checkOnlyConnection{checkErr: injected})
	if res == nil || !errors.Is(err, injected) {
		t.Fatalf("unreachable route = result %v, err %v", res, err)
	}

	live := &kamajiv1alpha1.TenantControlPlane{}
	if err = cl.Get(context.Background(), client.ObjectKeyFromObject(tcp), live); err != nil {
		t.Fatal(err)
	}

	if _, acked := live.Annotations[utilities.DataStoreEndpointObservedAnnotation]; acked {
		t.Fatal("route acknowledged although the check failed")
	}

	// A reachable, not-yet-acknowledged route is recorded and asks for a
	// one-second requeue because the TCP object changed.
	res, err = r.ackKubeDCDataStoreRoute(context.Background(), tcp, ds, checkOnlyConnection{})
	if err != nil || res == nil || res.RequeueAfter != time.Second {
		t.Fatalf("first acknowledgement = result %v, err %v", res, err)
	}

	if err = cl.Get(context.Background(), client.ObjectKeyFromObject(tcp), live); err != nil {
		t.Fatal(err)
	}

	if got := live.Annotations[utilities.DataStoreEndpointObservedAnnotation]; got != utilities.DataStoreEndpointFingerprint(ds) {
		t.Fatalf("datastore endpoint observation = %q", got)
	}

	// Already acknowledged: carry on with the reconcile.
	res, err = r.ackKubeDCDataStoreRoute(context.Background(), live, ds, checkOnlyConnection{})
	if err != nil || res != nil {
		t.Fatalf("steady state = result %v, err %v", res, err)
	}
}
