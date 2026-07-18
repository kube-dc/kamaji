package controllers

import (
	"context"
	"testing"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	"github.com/clastix/kamaji/internal/utilities"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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
