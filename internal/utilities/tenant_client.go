// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package utilities

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	clientcmdapiv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
)

func GetTenantClient(ctx context.Context, c client.Client, tenantControlPlane *kamajiv1alpha1.TenantControlPlane) (client.Client, error) {
	options := client.Options{}
	config, err := GetRESTClientConfig(ctx, c, tenantControlPlane)
	if err != nil {
		return nil, err
	}

	return client.New(config, options)
}

func GetTenantClientSet(ctx context.Context, client client.Client, tenantControlPlane *kamajiv1alpha1.TenantControlPlane) (*clientset.Clientset, error) {
	config, err := GetRESTClientConfig(ctx, client, tenantControlPlane)
	if err != nil {
		return nil, err
	}

	return clientset.NewForConfig(config)
}

func GetTenantKubeconfig(ctx context.Context, client client.Client, tenantControlPlane *kamajiv1alpha1.TenantControlPlane) (*clientcmdapiv1.Config, error) {
	secretKubeconfig := &corev1.Secret{}
	if err := client.Get(ctx, k8stypes.NamespacedName{Namespace: tenantControlPlane.GetNamespace(), Name: tenantControlPlane.Status.KubeConfig.Admin.SecretName}, secretKubeconfig); err != nil {
		return nil, err
	}

	secretKey := kubeadmconstants.SuperAdminKubeConfigFileName
	v, ok := tenantControlPlane.GetAnnotations()[kamajiv1alpha1.KubeconfigSecretKeyAnnotation]
	if ok && v != "" {
		secretKey = v
	}

	return DecodeKubeconfig(*secretKubeconfig, secretKey)
}

func GetRESTClientConfig(ctx context.Context, client client.Client, tenantControlPlane *kamajiv1alpha1.TenantControlPlane) (*restclient.Config, error) {
	kubeconfig, err := GetTenantKubeconfig(ctx, client, tenantControlPlane)
	if err != nil {
		return nil, err
	}

	// Resolve the route after reading the kubeconfig. An explicit selection is
	// authoritative; for legacy, unannotated TCPs we also tolerate an already
	// retired -ext Service by falling back to the upstream ClusterIP name. This
	// is required to upgrade clusters that completed the old namespace-wide
	// retirement before per-Service transition tokens existed.
	host, err := tenantClientHost(ctx, client, tenantControlPlane)
	if err != nil {
		return nil, err
	}
	// ServerName for TLS verification (always uses the original service name)
	serverName := fmt.Sprintf("%s.%s.svc.cluster.local", tenantControlPlane.GetName(), tenantControlPlane.GetNamespace())

	// Use external endpoint service (-ext) if LoadBalancer is assigned
	// This enables cross-VPC deployments where controller cannot reach Service ClusterIP
	// kube-dc automatically creates <service>-ext endpoints for LoadBalancer services

	config := &restclient.Config{
		Host: host,
		TLSClientConfig: restclient.TLSClientConfig{
			CAData:     kubeconfig.Clusters[0].Cluster.CertificateAuthorityData,
			CertData:   kubeconfig.AuthInfos[0].AuthInfo.ClientCertificateData,
			KeyData:    kubeconfig.AuthInfos[0].AuthInfo.ClientKeyData,
			ServerName: serverName,
		},
		Timeout: 10 * time.Second,
	}

	return config, nil
}

const (
	TenantClientEndpointAnnotation         = "network.kube-dc.com/tenant-client-endpoint"
	TenantClientEndpointClusterIP          = "cluster-ip"
	TenantClientEndpointObservedAnnotation = "network.kube-dc.com/tenant-client-endpoint-observed"
	DataStoreEndpointObservedAnnotation    = "network.kube-dc.com/datastore-endpoint-observed"
	EndpointModeDirect                     = "direct"
	TenantClientEndpointTokenAnnotation    = "network.kube-dc.com/tenant-client-endpoint-token"
	EndpointModeExternal                   = "external"
)

func tenantClientHost(
	ctx context.Context,
	cli client.Client,
	tenantControlPlane *kamajiv1alpha1.TenantControlPlane,
) (string, error) {
	// Upstream/default path: the stable Service name. Dual-homed control-plane
	// pods put this ClusterIP on the default VPC, so the Kamaji controller can
	// route to it directly.
	host := fmt.Sprintf("https://%s.%s.svc:%d",
		tenantControlPlane.GetName(),
		tenantControlPlane.GetNamespace(),
		tenantControlPlane.Spec.NetworkProfile.Port)

	// No LoadBalancer or an explicit direct selection uses the upstream name.
	if tenantControlPlane.Status.ControlPlaneEndpoint == "" ||
		TenantClientEndpointSelection(tenantControlPlane) == TenantClientEndpointClusterIP {
		return host, nil
	}

	// Legacy kube-dc path. Before per-Service transition tokens, stage retired
	// -ext namespace-wide. On upgrade those TCPs are necessarily unannotated.
	// Preserve the legacy route whenever its Service still exists, but if it is
	// definitively absent use the upstream ClusterIP so a Kamaji restart cannot
	// strand an already-migrated TCP. API read failures remain fail-closed: they
	// are not evidence that the compatibility endpoint is gone.
	extName := tenantControlPlane.GetName() + "-ext"
	ext := &corev1.Service{}
	if err := cli.Get(ctx, k8stypes.NamespacedName{
		Namespace: tenantControlPlane.GetNamespace(),
		Name:      extName,
	}, ext); err != nil {
		if apierrors.IsNotFound(err) {
			return host, nil
		}

		return "", fmt.Errorf("read legacy tenant API Service %s/%s: %w",
			tenantControlPlane.GetNamespace(), extName, err)
	}

	return fmt.Sprintf("https://%s.%s.svc.cluster.local:%d",
		extName,
		tenantControlPlane.GetNamespace(),
		tenantControlPlane.Spec.NetworkProfile.Port), nil
}

func TenantClientEndpointSelection(tenantControlPlane *kamajiv1alpha1.TenantControlPlane) string {
	if tenantControlPlane.GetAnnotations()[TenantClientEndpointAnnotation] == TenantClientEndpointClusterIP {
		return TenantClientEndpointClusterIP
	}

	return EndpointModeExternal
}

func DataStoreEndpointFingerprint(dataStore *kamajiv1alpha1.DataStore) string {
	endpoints := append([]string(nil), dataStore.Spec.Endpoints...)
	management := append([]string(nil), dataStore.Spec.ManagementEndpoints...)
	sort.Strings(endpoints)
	sort.Strings(management)
	canonical := strings.Join(endpoints, "\x00") + "\x01" + strings.Join(management, "\x00")
	sum := sha256.Sum256([]byte(canonical))

	return fmt.Sprintf("%x", sum[:])
}
