package utilities

import (
	"context"
	"errors"
	"testing"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type getErrorClient struct {
	client.Client
}

func (c getErrorClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("injected API read failure")
}

func tenantClientFake(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func tenantClientTCP() *kamajiv1alpha1.TenantControlPlane {
	return &kamajiv1alpha1.TenantControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "b1-cp", Namespace: "shalb-dev"},
		Spec: kamajiv1alpha1.TenantControlPlaneSpec{
			NetworkProfile: kamajiv1alpha1.NetworkProfileSpec{Port: 6443},
		},
	}
}

func TestTenantClientHostKeepsLegacyExternalEndpointByDefault(t *testing.T) {
	tcp := tenantClientTCP()
	tcp.Status.ControlPlaneEndpoint = "100.65.5.46:6443"
	ext := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "b1-cp-ext", Namespace: "shalb-dev"}}
	got, err := tenantClientHost(context.Background(), tenantClientFake(t, ext), tcp)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://b1-cp-ext.shalb-dev.svc.cluster.local:6443"
	if got != want {
		t.Fatalf("legacy Ready TCP host = %q, want %q", got, want)
	}
}

func TestTenantClientHostUsesClusterIPOnlyOnExplicitOptIn(t *testing.T) {
	tcp := tenantClientTCP()
	tcp.Status.ControlPlaneEndpoint = "100.65.5.46:6443"
	tcp.Annotations = map[string]string{
		TenantClientEndpointAnnotation: TenantClientEndpointClusterIP,
	}
	got, err := tenantClientHost(context.Background(), tenantClientFake(t), tcp)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://b1-cp.shalb-dev.svc:6443"
	if got != want {
		t.Fatalf("opted-in TCP host = %q, want %q", got, want)
	}
}

func TestTenantClientHostWithoutLoadBalancerUsesUpstreamService(t *testing.T) {
	tcp := tenantClientTCP()
	got, err := tenantClientHost(context.Background(), tenantClientFake(t), tcp)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://b1-cp.shalb-dev.svc:6443"
	if got != want {
		t.Fatalf("ClusterIP TCP host = %q, want %q", got, want)
	}
}

func TestTenantClientHostUsesClusterIPWhenLegacyAliasWasAlreadyRetired(t *testing.T) {
	tcp := tenantClientTCP()
	tcp.Status.ControlPlaneEndpoint = "100.65.5.46:6443"

	got, err := tenantClientHost(context.Background(), tenantClientFake(t), tcp)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://b1-cp.shalb-dev.svc:6443"
	if got != want {
		t.Fatalf("already-retired TCP host = %q, want %q", got, want)
	}
}

func TestTenantClientHostFailsClosedOnLegacyAliasReadError(t *testing.T) {
	tcp := tenantClientTCP()
	tcp.Status.ControlPlaneEndpoint = "100.65.5.46:6443"
	cli := getErrorClient{Client: tenantClientFake(t)}

	if _, err := tenantClientHost(context.Background(), cli, tcp); err == nil {
		t.Fatal("legacy alias API read error must not be mistaken for a retired alias")
	}
}
