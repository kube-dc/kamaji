// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"bytes"
	"context"
	"fmt"

	jsonpatchv5 "github.com/evanphx/json-patch/v5"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	"github.com/clastix/kamaji/internal/constants"
	"github.com/clastix/kamaji/internal/kubeadm"
	"github.com/clastix/kamaji/internal/resources"
	addon_utils "github.com/clastix/kamaji/internal/resources/addons/utils"
	"github.com/clastix/kamaji/internal/resources/utils"
	"github.com/clastix/kamaji/internal/utilities"
)

// kubeProxyConfigConfKey is the key in the kube-proxy ConfigMap that holds the
// KubeProxyConfiguration YAML payload (kubeadm-managed).
const kubeProxyConfigConfKey = "config.conf"

type KubeProxy struct {
	Client client.Client

	serviceAccount     *corev1.ServiceAccount
	clusterRoleBinding *rbacv1.ClusterRoleBinding
	role               *rbacv1.Role
	roleBinding        *rbacv1.RoleBinding
	configMap          *corev1.ConfigMap
	daemonSet          *appsv1.DaemonSet
}

func (k *KubeProxy) GetHistogram() prometheus.Histogram {
	kubeProxyCollector = resources.LazyLoadHistogramFromResource(kubeProxyCollector, k)

	return kubeProxyCollector
}

func (k *KubeProxy) Define(context.Context, *kamajiv1alpha1.TenantControlPlane) error {
	k.clusterRoleBinding = &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: kubeadm.KubeProxyClusterRoleBindingName,
		},
	}
	k.serviceAccount = &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeadm.KubeProxyServiceAccountName,
			Namespace: kubeadm.KubeSystemNamespace,
		},
	}
	k.role = &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeadm.KubeProxyConfigMapRoleName,
			Namespace: kubeadm.KubeSystemNamespace,
		},
	}
	k.roleBinding = &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeadm.KubeProxyConfigMapRoleName,
			Namespace: kubeadm.KubeSystemNamespace,
		},
	}
	k.configMap = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeadm.KubeProxyConfigMap,
			Namespace: kubeadm.KubeSystemNamespace,
		},
	}
	k.daemonSet = &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeadm.KubeProxyName,
			Namespace: kubeadm.KubeSystemNamespace,
		},
	}

	return nil
}

func (k *KubeProxy) ShouldCleanup(tenantControlPlane *kamajiv1alpha1.TenantControlPlane) bool {
	return tenantControlPlane.Spec.Addons.KubeProxy == nil && tenantControlPlane.Status.Addons.KubeProxy.Enabled
}

func (k *KubeProxy) CleanUp(ctx context.Context, tcp *kamajiv1alpha1.TenantControlPlane) (bool, error) {
	logger := log.FromContext(ctx, "resource", "kubeadm_addons", "addon", k.GetName())

	tenantClient, err := utilities.GetTenantClient(ctx, k.Client, tcp)
	if err != nil {
		logger.Error(err, "cannot generate Tenant client")

		return false, err
	}

	var deleted bool

	for _, obj := range []client.Object{k.serviceAccount, k.clusterRoleBinding, k.role, k.roleBinding, k.configMap, k.daemonSet} {
		if err = tenantClient.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, obj); err != nil {
			if k8serrors.IsNotFound(err) {
				continue
			}
		}
		// Skipping deletion:
		// the kubeproxy addons is not managed by Kamaji.
		if labels := obj.GetLabels(); labels == nil || labels[constants.ProjectNameLabelKey] != constants.ProjectNameLabelValue {
			continue
		}

		if err = tenantClient.Delete(ctx, obj); err != nil {
			if k8serrors.IsNotFound(err) {
				continue
			}

			return false, err
		}

		deleted = true
	}

	return deleted, nil
}

func (k *KubeProxy) CreateOrUpdate(ctx context.Context, tcp *kamajiv1alpha1.TenantControlPlane) (controllerutil.OperationResult, error) {
	if tcp.Spec.Addons.KubeProxy == nil {
		return controllerutil.OperationResultNone, nil
	}

	logger := log.FromContext(ctx, "addon", k.GetName())

	tenantClient, err := utilities.GetTenantClient(ctx, k.Client, tcp)
	if err != nil {
		logger.Error(err, "cannot generate Tenant client")

		return controllerutil.OperationResultNone, err
	}

	if err = k.decodeManifests(ctx, tcp); err != nil {
		logger.Error(err, "manifest decoding failed")

		return controllerutil.OperationResultNone, err
	}

	if err = k.applyConfigJSONPatches(tcp.Spec.Addons.KubeProxy.ConfigurationJSONPatches); err != nil {
		logger.Error(err, "kube-proxy config.conf JSON patching failed")

		return controllerutil.OperationResultNone, err
	}

	k.stampConfigChecksumOnDaemonSet()

	var operationResult controllerutil.OperationResult

	reconciliationResult := controllerutil.OperationResultNone
	// ClusterRoleBinding
	operationResult, err = k.mutateClusterRoleBinding(ctx, tenantClient)
	if err != nil {
		logger.Error(err, "ClusterRoleBinding reconciliation failed")

		return controllerutil.OperationResultNone, err
	}
	reconciliationResult = utils.UpdateOperationResult(reconciliationResult, operationResult)
	// ConfigMap MUST be persisted before the DaemonSet: mutateDaemonSet
	// carries the kamaji.clastix.io/checksum pod-template annotation
	// (stampConfigChecksumOnDaemonSet), and once that lands the DaemonSet
	// controller rolls kube-proxy pods. If the DaemonSet went first, a new
	// pod could start and mount the OLD config.conf while already carrying
	// the NEW pod-template hash; the subsequent ConfigMap update would then
	// not re-roll it (hash unchanged), leaving that node on stale config.
	// Writing the ConfigMap first closes that window.
	operationResult, err = k.mutateConfigMap(ctx, tenantClient)
	if err != nil {
		logger.Error(err, "ConfigMap reconciliation failed")

		return controllerutil.OperationResultNone, err
	}
	reconciliationResult = utils.UpdateOperationResult(reconciliationResult, operationResult)
	// DaemonSet (after ConfigMap — see ordering note above)
	operationResult, err = k.mutateDaemonSet(ctx, tenantClient)
	if err != nil {
		logger.Error(err, "DaemonSet reconciliation failed")

		return controllerutil.OperationResultNone, err
	}
	reconciliationResult = utils.UpdateOperationResult(reconciliationResult, operationResult)
	// RoleBinding
	operationResult, err = k.mutateRoleBinding(ctx, tenantClient)
	if err != nil {
		logger.Error(err, "RoleBinding reconciliation failed")

		return controllerutil.OperationResultNone, err
	}
	reconciliationResult = utils.UpdateOperationResult(reconciliationResult, operationResult)
	// Role
	operationResult, err = k.mutateRole(ctx, tenantClient)
	if err != nil {
		logger.Error(err, "Role reconciliation failed")

		return controllerutil.OperationResultNone, err
	}
	reconciliationResult = utils.UpdateOperationResult(reconciliationResult, operationResult)
	// ServiceAccount
	operationResult, err = k.mutateServiceAccount(ctx, tenantClient)
	if err != nil {
		logger.Error(err, "ServiceAccount reconciliation failed")

		return controllerutil.OperationResultNone, err
	}
	reconciliationResult = utils.UpdateOperationResult(reconciliationResult, operationResult)

	return reconciliationResult, nil
}

func (k *KubeProxy) GetName() string {
	return "kube-proxy"
}

func (k *KubeProxy) ShouldStatusBeUpdated(_ context.Context, tcp *kamajiv1alpha1.TenantControlPlane) bool {
	return tcp.Spec.Addons.KubeProxy != nil && !tcp.Status.Addons.KubeProxy.Enabled
}

func (k *KubeProxy) UpdateTenantControlPlaneStatus(_ context.Context, tcp *kamajiv1alpha1.TenantControlPlane) error {
	tcp.Status.Addons.KubeProxy.Enabled = tcp.Spec.Addons.KubeProxy != nil
	tcp.Status.Addons.KubeProxy.LastUpdate = metav1.Now()

	return nil
}

func (k *KubeProxy) mutateClusterRoleBinding(ctx context.Context, tenantClient client.Client) (controllerutil.OperationResult, error) {
	crb := &rbacv1.ClusterRoleBinding{}
	crb.SetName(k.clusterRoleBinding.GetName())

	defer func() {
		k.clusterRoleBinding.SetUID(crb.GetUID())
	}()

	return utilities.CreateOrUpdateWithConflict(ctx, tenantClient, crb, func() error {
		crb.SetLabels(utilities.MergeMaps(crb.GetLabels(), k.clusterRoleBinding.GetLabels()))
		crb.SetAnnotations(utilities.MergeMaps(crb.GetAnnotations(), k.clusterRoleBinding.GetAnnotations()))
		crb.Subjects = k.clusterRoleBinding.Subjects
		crb.RoleRef = k.clusterRoleBinding.RoleRef

		return nil
	})
}

func (k *KubeProxy) mutateServiceAccount(ctx context.Context, tenantClient client.Client) (controllerutil.OperationResult, error) {
	sa := &corev1.ServiceAccount{}
	sa.SetName(k.serviceAccount.GetName())
	sa.SetNamespace(k.serviceAccount.GetNamespace())

	return utilities.CreateOrUpdateWithConflict(ctx, tenantClient, sa, func() error {
		sa.SetLabels(utilities.MergeMaps(sa.GetLabels(), k.serviceAccount.GetLabels()))
		sa.SetAnnotations(utilities.MergeMaps(sa.GetAnnotations(), k.serviceAccount.GetAnnotations()))

		return controllerutil.SetControllerReference(k.clusterRoleBinding, sa, tenantClient.Scheme())
	})
}

func (k *KubeProxy) mutateRole(ctx context.Context, tenantClient client.Client) (controllerutil.OperationResult, error) {
	r := &rbacv1.Role{}
	r.SetName(k.role.GetName())
	r.SetNamespace(k.role.GetNamespace())

	return utilities.CreateOrUpdateWithConflict(ctx, tenantClient, r, func() error {
		r.SetLabels(utilities.MergeMaps(r.GetLabels(), k.role.GetLabels()))
		r.SetAnnotations(utilities.MergeMaps(r.GetAnnotations(), k.role.GetAnnotations()))
		r.Rules = k.role.Rules

		return controllerutil.SetControllerReference(k.clusterRoleBinding, r, tenantClient.Scheme())
	})
}

func (k *KubeProxy) mutateRoleBinding(ctx context.Context, tenantClient client.Client) (controllerutil.OperationResult, error) {
	rb := &rbacv1.RoleBinding{}
	rb.SetName(k.roleBinding.GetName())
	rb.SetNamespace(k.roleBinding.GetNamespace())

	return utilities.CreateOrUpdateWithConflict(ctx, tenantClient, rb, func() error {
		rb.SetLabels(utilities.MergeMaps(rb.GetLabels(), k.roleBinding.GetLabels()))
		rb.SetAnnotations(utilities.MergeMaps(rb.GetAnnotations(), k.roleBinding.GetAnnotations()))
		if len(rb.Subjects) == 0 {
			rb.Subjects = make([]rbacv1.Subject, 1)
		}
		rb.Subjects[0].Kind = k.roleBinding.Subjects[0].Kind
		rb.Subjects[0].APIGroup = rbacv1.GroupName
		rb.Subjects[0].Name = k.roleBinding.Subjects[0].Name
		rb.RoleRef = k.roleBinding.RoleRef

		return controllerutil.SetControllerReference(k.clusterRoleBinding, rb, tenantClient.Scheme())
	})
}

func (k *KubeProxy) mutateConfigMap(ctx context.Context, tenantClient client.Client) (controllerutil.OperationResult, error) {
	cm := &corev1.ConfigMap{}
	cm.SetName(k.configMap.GetName())
	cm.SetNamespace(k.configMap.GetNamespace())

	return utilities.CreateOrUpdateWithConflict(ctx, tenantClient, cm, func() error {
		cm.SetLabels(utilities.MergeMaps(cm.GetLabels(), k.configMap.GetLabels()))
		cm.SetAnnotations(utilities.MergeMaps(cm.GetAnnotations(), k.configMap.GetAnnotations()))
		cm.Data = k.configMap.Data

		return nil
	})
}

func (k *KubeProxy) mutateDaemonSet(ctx context.Context, tenantClient client.Client) (controllerutil.OperationResult, error) {
	var ds appsv1.DaemonSet
	ds.Name = k.daemonSet.Name
	ds.Namespace = k.daemonSet.Namespace

	if err := tenantClient.Get(ctx, client.ObjectKeyFromObject(&ds), &ds); err != nil {
		if k8serrors.IsNotFound(err) {
			return utilities.CreateOrUpdateWithConflict(ctx, tenantClient, k.daemonSet, func() error {
				return controllerutil.SetControllerReference(k.clusterRoleBinding, k.daemonSet, tenantClient.Scheme())
			})
		}

		return controllerutil.OperationResultNone, err
	}

	if err := controllerutil.SetControllerReference(k.clusterRoleBinding, k.daemonSet, tenantClient.Scheme()); err != nil {
		return controllerutil.OperationResultNone, err
	}
	//nolint:staticcheck
	return controllerutil.OperationResultNone, tenantClient.Patch(ctx, k.daemonSet, client.Apply, client.FieldOwner("kamaji"), client.ForceOwnership)
}

// applyConfigJSONPatches applies the RFC-6902 JSON patches from
// `tcp.Spec.Addons.KubeProxy.ConfigurationJSONPatches` to the kube-proxy
// KubeProxyConfiguration YAML in `k.configMap.Data["config.conf"]`. The YAML
// is converted to JSON for patching and back to YAML before being written, so
// the kubelet/kube-proxy still see canonical YAML. Idempotent if patches is empty.
func (k *KubeProxy) applyConfigJSONPatches(patches kamajiv1alpha1.JSONPatches) error {
	if len(patches) == 0 {
		return nil
	}

	rawYAML, ok := k.configMap.Data[kubeProxyConfigConfKey]
	if !ok {
		return fmt.Errorf("kube-proxy ConfigMap has no %q key to patch", kubeProxyConfigConfKey)
	}

	asJSON, err := yaml.YAMLToJSON([]byte(rawYAML))
	if err != nil {
		return fmt.Errorf("converting %s YAML to JSON: %w", kubeProxyConfigConfKey, err)
	}

	patchBytes, err := patches.ToJSON()
	if err != nil {
		return fmt.Errorf("encoding kube-proxy JSON patches: %w", err)
	}

	patch, err := jsonpatchv5.DecodePatch(patchBytes)
	if err != nil {
		return fmt.Errorf("decoding kube-proxy JSON patches: %w", err)
	}

	patched, err := patch.Apply(asJSON)
	if err != nil {
		return fmt.Errorf("applying kube-proxy JSON patches to %s: %w", kubeProxyConfigConfKey, err)
	}

	patchedYAML, err := yaml.JSONToYAML(patched)
	if err != nil {
		return fmt.Errorf("converting patched %s back to YAML: %w", kubeProxyConfigConfKey, err)
	}

	k.configMap.Data[kubeProxyConfigConfKey] = string(patchedYAML)

	return nil
}

// stampConfigChecksumOnDaemonSet computes a sorted-map checksum of the kube-proxy
// ConfigMap data and stamps it on the DaemonSet pod-template annotations under
// the standard `kamaji.clastix.io/checksum` key. This guarantees that any change
// to `config.conf` (including JSON-patch additions/removals) bumps the pod-template
// hash, forcing the kubelet on every worker to roll the kube-proxy pod and
// pick up the new configuration.
func (k *KubeProxy) stampConfigChecksumOnDaemonSet() {
	checksum := utilities.CalculateMapChecksum(k.configMap.Data)

	if k.daemonSet.Spec.Template.Annotations == nil {
		k.daemonSet.Spec.Template.Annotations = map[string]string{}
	}

	k.daemonSet.Spec.Template.Annotations[constants.Checksum] = checksum
}

func (k *KubeProxy) decodeManifests(ctx context.Context, tcp *kamajiv1alpha1.TenantControlPlane) error {
	tcpClient, config, err := resources.GetKubeadmManifestDeps(ctx, k.Client, tcp)
	if err != nil {
		return fmt.Errorf("unable to create manifests dependencies: %w", err)
	}
	// If the kube-proxy addon has overrides, adding it to the kubeadm parameters
	config.Parameters.KubeProxyOptions = &kubeadm.AddonOptions{}

	if len(tcp.Spec.Addons.KubeProxy.ImageRepository) > 0 {
		config.Parameters.KubeProxyOptions.Repository = tcp.Spec.Addons.KubeProxy.ImageRepository
	} else {
		config.Parameters.KubeProxyOptions.Repository = "registry.k8s.io"
	}

	if len(tcp.Spec.Addons.KubeProxy.ImageTag) > 0 {
		config.Parameters.KubeProxyOptions.Tag = tcp.Spec.Addons.KubeProxy.ImageTag
	} else {
		config.Parameters.KubeProxyOptions.Tag = tcp.Spec.Kubernetes.Version
	}

	manifests, err := kubeadm.AddKubeProxy(tcpClient, config)
	if err != nil {
		return fmt.Errorf("unable to generate manifests: %w", err)
	}

	parts := bytes.Split(manifests, []byte("---"))

	if err = utilities.DecodeFromYAML(string(parts[1]), k.serviceAccount); err != nil {
		return fmt.Errorf("unable to decode ServiceAccount manifest: %w", err)
	}
	addon_utils.SetKamajiManagedLabels(k.serviceAccount)

	if err = utilities.DecodeFromYAML(string(parts[2]), k.clusterRoleBinding); err != nil {
		return fmt.Errorf("unable to decode ClusterRoleBinding manifest: %w", err)
	}
	addon_utils.SetKamajiManagedLabels(k.clusterRoleBinding)

	if err = utilities.DecodeFromYAML(string(parts[3]), k.role); err != nil {
		return fmt.Errorf("unable to decode Role manifest: %w", err)
	}
	addon_utils.SetKamajiManagedLabels(k.role)

	if err = utilities.DecodeFromYAML(string(parts[4]), k.roleBinding); err != nil {
		return fmt.Errorf("unable to decode RoleBinding manifest: %w", err)
	}
	addon_utils.SetKamajiManagedLabels(k.roleBinding)

	if err = utilities.DecodeFromYAML(string(parts[5]), k.configMap); err != nil {
		return fmt.Errorf("unable to decode ConfigMap manifest: %w", err)
	}
	addon_utils.SetKamajiManagedLabels(k.configMap)

	if err = utilities.DecodeFromYAML(string(parts[6]), k.daemonSet); err != nil {
		return fmt.Errorf("unable to decode DaemonSet manifest: %w", err)
	}
	addon_utils.SetKamajiManagedLabels(k.daemonSet)

	return nil
}
