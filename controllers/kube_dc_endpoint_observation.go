// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	"github.com/clastix/kamaji/internal/datastore"
	"github.com/clastix/kamaji/internal/utilities"
)

// observeKubeDCDataStoreEndpoint records the exact DataStore route already
// checked by the caller under the per-TCP mutex. Tenant API endpoint
// observation belongs to the soot manager, which owns that long-lived client.
func (r *TenantControlPlaneReconciler) observeKubeDCDataStoreEndpoint(
	ctx context.Context,
	tcp *kamajiv1alpha1.TenantControlPlane,
	ds *kamajiv1alpha1.DataStore,
) (bool, error) {
	datastoreObservation := utilities.DataStoreEndpointFingerprint(ds)

	annotations := tcp.GetAnnotations()
	if annotations[utilities.DataStoreEndpointObservedAnnotation] == datastoreObservation {
		return false, nil
	}

	base := tcp.DeepCopy()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[utilities.DataStoreEndpointObservedAnnotation] = datastoreObservation
	tcp.SetAnnotations(annotations)
	if err := r.Client.Patch(ctx, tcp, client.MergeFrom(base)); err != nil {
		return false, fmt.Errorf("acknowledge kube-dc endpoint selections: %w", err)
	}

	return true, nil
}

// ackKubeDCDataStoreRoute proves that the DataStore route selected for this
// TenantControlPlane is usable and records its fingerprint on the TCP.
//
// DataStore Ready may be stale across an endpoint transition, so the exact
// management route is checked before its fingerprint is acknowledged;
// k8-manager keeps the compatibility alias until every live TCP has published
// this applied-state proof.
//
// The live check runs only while the current fingerprint is not yet
// acknowledged: once it is, every reconcile must NOT re-check the datastore,
// or N TenantControlPlanes turn each unrelated reconcile (upstream now fans
// datastore Secret changes and certificate rotations into reconciles) into an
// N-connection burst against a shared datastore. Ongoing reachability is the
// datastore controller's and the rendering path's concern.
//
// A non-nil result must be returned by the caller as-is: paired with an error
// when the route is unreachable or the acknowledgement could not be written,
// or a one-second requeue when the fingerprint was just recorded (the TCP
// object changed underneath the reconcile). A nil result means "carry on".
func (r *TenantControlPlaneReconciler) ackKubeDCDataStoreRoute(
	ctx context.Context,
	tcp *kamajiv1alpha1.TenantControlPlane,
	ds *kamajiv1alpha1.DataStore,
	dsConnection datastore.Connection,
) (*ctrl.Result, error) {
	if tcp.GetAnnotations()[utilities.DataStoreEndpointObservedAnnotation] == utilities.DataStoreEndpointFingerprint(ds) {
		return nil, nil
	}

	if err := dsConnection.Check(ctx); err != nil {
		log.FromContext(ctx).Error(err, "selected DataStore route is not reachable")

		return &ctrl.Result{}, err
	}

	changed, err := r.observeKubeDCDataStoreEndpoint(ctx, tcp, ds)
	if err != nil {
		return &ctrl.Result{}, err
	}

	if changed {
		return &ctrl.Result{RequeueAfter: time.Second}, nil
	}

	return nil, nil
}
