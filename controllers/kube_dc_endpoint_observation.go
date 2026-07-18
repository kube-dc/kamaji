package controllers

import (
	"context"
	"fmt"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	"github.com/clastix/kamaji/internal/utilities"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
