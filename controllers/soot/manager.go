// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package soot

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	"github.com/clastix/kamaji/controllers/finalizers"
	"github.com/clastix/kamaji/controllers/soot/controllers"
	kamajierrors "github.com/clastix/kamaji/controllers/soot/controllers/errors"
	"github.com/clastix/kamaji/controllers/utils"
	"github.com/clastix/kamaji/internal/resources"
	"github.com/clastix/kamaji/internal/utilities"
)

type sootItem struct {
	certificateSha string
	triggers       []chan event.GenericEvent
	cancelFn       context.CancelFunc
	completedCh    chan struct{}
	// endpointObservation identifies the exact tenant API route used to build
	// this long-lived manager. A changed transition token forces a rebuild.
	endpointObservation string
}

type sootMap map[string]sootItem

const (
	sootManagerAnnotation       = "kamaji.clastix.io/soot"
	sootManagerFailedAnnotation = "failed"
)

type Manager struct {
	sootMap sootMap
	// sootManagerErrChan is the channel that is going to be used
	// when the soot manager cannot start due to any kind of problem.
	sootManagerErrChan chan event.GenericEvent

	MigrateCABundle         []byte
	MigrateServiceName      string
	MigrateServiceNamespace string
	AdminClient             client.Client
}

// retrieveTenantControlPlane is the function used to let an underlying controller of the soot manager
// to retrieve its parent TenantControlPlane definition, required to understand which actions must be performed.
func (m *Manager) retrieveTenantControlPlane(ctx context.Context, request reconcile.Request) utils.TenantControlPlaneRetrievalFn {
	return func() (*kamajiv1alpha1.TenantControlPlane, error) {
		tcp := &kamajiv1alpha1.TenantControlPlane{}

		if err := m.AdminClient.Get(ctx, request.NamespacedName, tcp); err != nil {
			return nil, err
		}

		if utils.IsPaused(tcp) {
			return nil, kamajierrors.ErrPausedReconciliation
		}

		return tcp, nil
	}
}

// If the TenantControlPlane is deleted we have to free up memory by stopping the soot manager:
// this is made possible by retrieving the cancel function of the soot manager context to cancel it.
// watchdogProbeInterval is how often the per-TCP watchdog probes the
// tenant API. 30s matches Kamaji's default reconcile cadence for healthy
// clusters. Each probe is a single /version request with its own 5s client
// timeout (see tenantHealthWatchdog), so a probe can never outlast the
// interval and a healthy cluster stays unaffected.
const watchdogProbeInterval = 30 * time.Second

// watchdogMaxFailures is the number of consecutive probe failures
// after which the per-TCP soot manager is torn down. With 30s spacing,
// this is ~2.5 minutes of unbroken unreachability — long enough to
// ride out a kube-apiserver restart on the tenant side, short enough
// that a permanently-unreachable tenant doesn't keep the soot
// informers wedged for hours.
const watchdogMaxFailures = 5

// pausedRequeueInterval bounds how long a TenantControlPlane whose rendered
// Deployment is scaled to zero waits before the soot manager checks again.
// Nothing guarantees an event when the orchestrator scales it back up
// without touching the TCP, so this is the wake-up backstop.
const pausedRequeueInterval = 30 * time.Second

// tenantHealthWatchdog probes the per-TCP tenant API every
// watchdogProbeInterval. After watchdogMaxFailures consecutive
// failures it annotates the TCP with the soot-failed marker and
// cancels the per-TCP context, which causes the existing soot manager
// goroutine to exit and the next reconcile to rebuild the soot
// manager from scratch.
//
// Background. The per-TCP source.Kind informers that the soot
// controllers register against the tenant API retry their initial
// list/watch indefinitely (controller-runtime behavior). When the
// tenant apiserver becomes permanently unreachable — e.g. the tenant
// org is suspended and its kube-apiserver pods get scaled to 0 — the
// informers wedge. The downstream effect is that the
// `KubernetesDeploymentResource.computeStatus` reconciler can't
// refresh `tcp.Status.KubernetesResources.Version.Status` (it depends
// on the same tenant connection in the soot controllers), so the
// status never transitions to `VersionNotReady` or `VersionSleeping`,
// and the soot manager's Reconcile-side cleanup paths
// (lines ~280, ~290 in this file) never trigger. The watchdog closes
// that loop by detecting the unreachability locally and forcing the
// rebuild.
//
// On every successful probe the failure counter resets, so transient
// network blips do not trigger a tear-down.
func (m *Manager) tenantHealthWatchdog(
	ctx context.Context,
	request reconcile.Request,
	tcpRest *rest.Config,
	cancelFn context.CancelFunc,
) {
	logger := log.FromContext(ctx).WithValues("watchdog", request.String())
	probeCfg := rest.CopyConfig(tcpRest)
	probeCfg.Timeout = 5 * time.Second

	clientset, err := kubernetes.NewForConfig(probeCfg)
	if err != nil {
		logger.Error(err, "watchdog: cannot build clientset, skipping health probes")

		return
	}

	failures := 0
	ticker := time.NewTicker(watchdogProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// ServerVersion() honors probeCfg.Timeout (set above) directly
		// via the rest.Config; no separate context needed.
		_, probeErr := clientset.Discovery().ServerVersion()

		if probeErr == nil {
			if failures > 0 {
				logger.V(2).Info("watchdog: tenant API reachable again", "previousFailures", failures)
			}
			failures = 0

			continue
		}

		failures++
		logger.V(2).Info("watchdog: tenant API probe failed", "failures", failures, "max", watchdogMaxFailures, "err", probeErr.Error())

		if failures < watchdogMaxFailures {
			continue
		}

		logger.Info("watchdog: tenant API unreachable beyond threshold, tearing down soot manager", "failures", failures)

		// Annotate the TCP so the next reconcile recognizes the
		// failed state and proceeds through the existing
		// `sootManagerFailedAnnotation` recovery path (which re-builds
		// the soot manager from scratch).
		if annoErr := m.retryTenantControlPlaneAnnotations(ctx, request, func(annotations map[string]string) {
			annotations[sootManagerAnnotation] = sootManagerFailedAnnotation
		}); annoErr != nil {
			logger.Error(annoErr, "watchdog: cannot annotate TCP for soot rebuild")
		}

		// Cancel the per-TCP context so the soot manager goroutine
		// exits and `completedCh` is closed; the parent Reconcile then
		// observes the failed annotation and rebuilds.
		cancelFn()

		return
	}
}

// deploymentPaused returns true when the rendered Deployment does not exist
// or has spec.replicas==0. This is the kube-dc lifecycle pause indicator: the
// orchestrator scales the Deployment to 0 without touching tcp.Spec, so
// tcp.Status can stay stale at Ready. Reading the Deployment directly avoids
// that staleness.
//
// It deliberately does NOT consult status.availableReplicas: upstream (#1193)
// renders readiness probes for the scheduler and controller-manager too, so a
// non-API container can make the whole pod unavailable while kube-apiserver
// still answers, and ordinary rollouts pass through availableReplicas==0 as
// well. Tenant API reachability is the watchdog's job, not this gate's.
func (m *Manager) deploymentPaused(ctx context.Context, tcp *kamajiv1alpha1.TenantControlPlane) bool {
	dep := &appsv1.Deployment{}
	if err := m.AdminClient.Get(ctx, types.NamespacedName{Namespace: tcp.Namespace, Name: tcp.Name}, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return true
		}
		log.FromContext(ctx).V(2).Info("cannot read Deployment for soot gate, assuming available", "err", err.Error())

		return false
	}

	return ptr.Deref(dep.Spec.Replicas, 0) == 0
}

func (m *Manager) cleanup(ctx context.Context, req reconcile.Request, tenantControlPlane *kamajiv1alpha1.TenantControlPlane) (err error) {
	if tenantControlPlane != nil && controllerutil.ContainsFinalizer(tenantControlPlane, finalizers.SootFinalizer) {
		defer func() {
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				tcp, tcpErr := m.retrieveTenantControlPlane(ctx, req)()
				if tcpErr != nil {
					return tcpErr
				}

				controllerutil.RemoveFinalizer(tcp, finalizers.SootFinalizer)

				return m.AdminClient.Update(ctx, tcp)
			})
		}()
	}

	tcpName := req.NamespacedName.String()

	v, ok := m.sootMap[tcpName]
	if !ok {
		return nil
	}

	v.cancelFn()
	// TODO(prometherion): the 10 seconds is an hardcoded number,
	// it's widely used across the code base as a timeout with the API Server.
	// Evaluate if we would need to make this configurable globally.
	deadlineCtx, deadlineFn := context.WithTimeout(ctx, 10*time.Second)
	defer deadlineFn()

	select {
	case _, open := <-v.completedCh:
		if !open {
			log.FromContext(ctx).Info("soot manager completed its process")

			break
		}
	case <-deadlineCtx.Done():
		log.FromContext(ctx).Error(deadlineCtx.Err(), "soot manager didn't exit to timeout")

		break
	}

	delete(m.sootMap, tcpName)

	return nil
}

func (m *Manager) retryTenantControlPlaneAnnotations(ctx context.Context, request reconcile.Request, modifierFn func(annotations map[string]string)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		tcp, err := m.retrieveTenantControlPlane(ctx, request)()
		if err != nil {
			return err
		}

		if tcp.Annotations == nil {
			tcp.Annotations = map[string]string{}
		}

		modifierFn(tcp.Annotations)

		tcp.SetAnnotations(tcp.Annotations)

		return m.AdminClient.Update(ctx, tcp)
	})
}

//nolint:maintidx,gocyclo
func (m *Manager) Reconcile(ctx context.Context, request reconcile.Request) (res reconcile.Result, err error) {
	logger := log.FromContext(ctx)
	// Retrieving the TenantControlPlane:
	// in case of deletion, we must be sure to properly remove from the memory the soot manager.
	tcp := &kamajiv1alpha1.TenantControlPlane{}
	if err = m.AdminClient.Get(ctx, request.NamespacedName, tcp); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, m.cleanup(ctx, request, nil)
		}

		return reconcile.Result{}, err
	}
	tcpStatus := ptr.Deref(tcp.Status.Kubernetes.Version.Status, kamajiv1alpha1.VersionProvisioning)
	// Handling finalizer if the TenantControlPlane is marked for deletion or scaled to zero:
	// the clean-up function is already taking care to stop the manager, if this exists.
	if tcp.GetDeletionTimestamp() != nil || tcpStatus == kamajiv1alpha1.VersionSleeping {
		if controllerutil.ContainsFinalizer(tcp, finalizers.SootFinalizer) {
			return reconcile.Result{}, m.cleanup(ctx, request, tcp)
		}

		return reconcile.Result{}, nil
	}
	// Triggering the reconciliation of the underlying controllers of
	// the soot manager if this is already registered.
	v, ok := m.sootMap[request.String()]
	if ok {
		switch {
		case tcp.Annotations != nil && tcp.Annotations[sootManagerAnnotation] == sootManagerFailedAnnotation:
			delete(m.sootMap, request.String())

			return reconcile.Result{}, m.retryTenantControlPlaneAnnotations(ctx, request, func(annotations map[string]string) {
				delete(annotations, sootManagerAnnotation)
			})
		case tcpStatus == kamajiv1alpha1.VersionCARotating:
			// The TenantControlPlane CA has been rotated, it means the running manager
			// must be restarted to avoid certificate signed by unknown authority errors.
			return reconcile.Result{}, m.cleanup(ctx, request, tcp)
		case tcpStatus == kamajiv1alpha1.VersionNotReady:
			// The TenantControlPlane is in non-ready mode, or marked for deletion:
			// we don't want to pollute with messages due to broken connection.
			// Once the TCP will be ready again, the event will be intercepted and the manager started back.
			return reconcile.Result{}, m.cleanup(ctx, request, tcp)
		case m.deploymentPaused(ctx, tcp):
			// External pause path: the rendered Deployment has been scaled to 0
			// but tcp.Spec replicas is non-zero, so tcpStatus may still report
			// Ready. Tear down the soot manager to stop informer retry spam
			// against the unreachable tenant API; the paused no-manager path
			// below requeues until replicas come back.
			return reconcile.Result{}, m.cleanup(ctx, request, tcp)
		case v.endpointObservation != tenantEndpointObservation(tcp):
			// Ordered after the lifecycle cases above on purpose: those must keep
			// their cleanup(..., tcp) semantics, while this is an in-place client
			// identity change.
			// GetRESTClientConfig is consumed by this long-lived manager, not the
			// parent TCP reconciler. Stop the old client before acknowledging or
			// using a new route; keep the finalizer while this is an in-place
			// transition.
			if cleanupErr := m.cleanup(ctx, request, nil); cleanupErr != nil {
				return reconcile.Result{}, cleanupErr
			}

			return reconcile.Result{RequeueAfter: time.Second}, nil
		case tcp.Status.KubeConfig.Admin.Checksum != v.certificateSha:
			// The stored kubeconfig to access the Tenant Control Plane has changed:
			// we need to clean-up and requeue to fetch the updated value.
			return reconcile.Result{RequeueAfter: time.Second}, m.cleanup(ctx, request, tcp)
		default:
			for _, trigger := range v.triggers {
				var shrunkTCP kamajiv1alpha1.TenantControlPlane

				shrunkTCP.Name = tcp.Name
				shrunkTCP.Namespace = tcp.Namespace

				utils.CoalesceTriggerChannel(trigger, shrunkTCP)
			}
		}

		return reconcile.Result{}, nil
	}
	// No need to start a soot manager if the TenantControlPlane is not ready:
	// enqueuing back is not required since we're going to get that event once ready.
	if tcpStatus == kamajiv1alpha1.VersionNotReady || tcpStatus == kamajiv1alpha1.VersionCARotating || tcpStatus == kamajiv1alpha1.VersionSleeping {
		logger.Info("skipping start of the soot manager for a not ready instance")

		return reconcile.Result{}, nil
	}
	// Independent of tcp.Status (which can be stale when an external
	// orchestrator scales the rendered Deployment to 0 without touching
	// tcp.Spec.ControlPlane.Deployment.Replicas), check the actual
	// Deployment. While it is scaled to zero the tenant API is unreachable
	// and starting a soot manager just generates retry spam for ~3 minutes
	// per cycle until cache-sync timeout. Skip, and requeue on a bounded
	// interval because nothing guarantees an event when replicas come back.
	if m.deploymentPaused(ctx, tcp) {
		logger.Info("skipping start of the soot manager: rendered Deployment is scaled to zero")

		return reconcile.Result{RequeueAfter: pausedRequeueInterval}, nil
	}
	// Generating the manager and starting it:
	// in case of any error, reconciling the request to start it back from the beginning.
	tcpRest, err := utilities.GetRESTClientConfig(ctx, m.AdminClient, tcp)
	if err != nil {
		if errors.Is(err, utilities.ErrMissingKubeconfigKey) {
			logger.Info("soot manager waiting for kubeconfig, enqueuing back")

			return reconcile.Result{RequeueAfter: time.Second}, nil
		}

		return reconcile.Result{}, err
	}
	// Setting the finalizer for the soot manager:
	// upon deletion the soot manager will be shut down prior the Deployment, avoiding logs pollution.
	if !controllerutil.ContainsFinalizer(tcp, finalizers.SootFinalizer) {
		_, finalizerErr := utilities.CreateOrUpdateWithConflict(ctx, m.AdminClient, tcp, func() error {
			controllerutil.AddFinalizer(tcp, finalizers.SootFinalizer)

			return nil
		})

		return reconcile.Result{RequeueAfter: time.Second}, finalizerErr
	}
	// Prove the selected route before publishing the exact transition token.
	// This also ensures the old manager has stopped before k8-manager can remove
	// the compatibility alias.
	probeCfg := rest.CopyConfig(tcpRest)
	probeCfg.Timeout = 5 * time.Second
	probeClient, probeErr := kubernetes.NewForConfig(probeCfg)
	if probeErr != nil {
		return reconcile.Result{}, probeErr
	}
	if _, probeErr = probeClient.Discovery().ServerVersion(); probeErr != nil {
		return reconcile.Result{}, fmt.Errorf("selected tenant API route is not reachable: %w", probeErr)
	}
	endpointObservation := tenantEndpointObservation(tcp)
	if tcp.GetAnnotations()[utilities.TenantClientEndpointObservedAnnotation] != endpointObservation {
		if err = m.retryTenantControlPlaneAnnotations(ctx, request, func(annotations map[string]string) {
			annotations[utilities.TenantClientEndpointObservedAnnotation] = endpointObservation
		}); err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{RequeueAfter: time.Second}, nil
	}

	tcpCtx, tcpCancelFn := context.WithCancel(ctx)
	defer func() {
		// If the reconciliation fails, we don't need to get a potential dangling goroutine.
		if err != nil {
			tcpCancelFn()
		}
	}()

	mgr, err := controllerruntime.NewManager(tcpRest, controllerruntime.Options{
		Logger: log.Log.WithName(fmt.Sprintf("soot_%s_%s", tcp.GetNamespace(), tcp.GetName())),
		Scheme: m.AdminClient.Scheme(),
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		NewClient: func(config *rest.Config, opts client.Options) (client.Client, error) {
			opts.Scheme = m.AdminClient.Scheme()

			return client.New(config, opts)
		},
	})
	if err != nil {
		return reconcile.Result{}, err
	}
	//
	// Register all the controllers of the soot here:
	//
	// Generate unique controller name prefix from TenantControlPlane to avoid metric conflicts
	controllerNamePrefix := fmt.Sprintf("%s-%s", tcp.GetNamespace(), tcp.GetName())

	writePermissions := &controllers.WritePermissions{
		Logger:                    mgr.GetLogger().WithName("writePermissions"),
		Client:                    mgr.GetClient(),
		GetTenantControlPlaneFunc: m.retrieveTenantControlPlane(tcpCtx, request),
		WebhookNamespace:          m.MigrateServiceNamespace,
		WebhookServiceName:        m.MigrateServiceName,
		WebhookCABundle:           m.MigrateCABundle,
		TriggerChannel:            make(chan event.GenericEvent, utils.CoalesceTriggerChannelBufferSize),
		ControllerName:            fmt.Sprintf("%s-writepermissions", controllerNamePrefix),
	}
	if err = writePermissions.SetupWithManager(mgr); err != nil {
		return reconcile.Result{}, err
	}

	migrate := &controllers.Migrate{
		WebhookNamespace:          m.MigrateServiceNamespace,
		WebhookServiceName:        m.MigrateServiceName,
		WebhookCABundle:           m.MigrateCABundle,
		GetTenantControlPlaneFunc: m.retrieveTenantControlPlane(tcpCtx, request),
		Client:                    mgr.GetClient(),
		Logger:                    mgr.GetLogger().WithName("migrate"),
		TriggerChannel:            make(chan event.GenericEvent, utils.CoalesceTriggerChannelBufferSize),
		ControllerName:            fmt.Sprintf("%s-migrate", controllerNamePrefix),
	}
	if err = migrate.SetupWithManager(mgr); err != nil {
		return reconcile.Result{}, err
	}

	konnectivityAgent := &controllers.KonnectivityAgent{
		AdminClient:               m.AdminClient,
		GetTenantControlPlaneFunc: m.retrieveTenantControlPlane(tcpCtx, request),
		Logger:                    mgr.GetLogger().WithName("konnectivity_agent"),
		TriggerChannel:            make(chan event.GenericEvent, utils.CoalesceTriggerChannelBufferSize),
		ControllerName:            fmt.Sprintf("%s-konnectivity", controllerNamePrefix),
	}
	if err = konnectivityAgent.SetupWithManager(mgr); err != nil {
		return reconcile.Result{}, err
	}

	kubeProxy := &controllers.KubeProxy{
		AdminClient:               m.AdminClient,
		GetTenantControlPlaneFunc: m.retrieveTenantControlPlane(tcpCtx, request),
		Logger:                    mgr.GetLogger().WithName("kube_proxy"),
		TriggerChannel:            make(chan event.GenericEvent, utils.CoalesceTriggerChannelBufferSize),
		ControllerName:            fmt.Sprintf("%s-kubeproxy", controllerNamePrefix),
	}
	if err = kubeProxy.SetupWithManager(mgr); err != nil {
		return reconcile.Result{}, err
	}

	coreDNS := &controllers.CoreDNS{
		AdminClient:               m.AdminClient,
		GetTenantControlPlaneFunc: m.retrieveTenantControlPlane(tcpCtx, request),
		Logger:                    mgr.GetLogger().WithName("coredns"),
		TriggerChannel:            make(chan event.GenericEvent, utils.CoalesceTriggerChannelBufferSize),
		ControllerName:            fmt.Sprintf("%s-coredns", controllerNamePrefix),
	}
	if err = coreDNS.SetupWithManager(mgr); err != nil {
		return reconcile.Result{}, err
	}

	uploadKubeadmConfig := &controllers.KubeadmPhase{
		GetTenantControlPlaneFunc: m.retrieveTenantControlPlane(tcpCtx, request),
		Phase: &resources.KubeadmPhase{
			Client: m.AdminClient,
			Phase:  resources.PhaseUploadConfigKubeadm,
		},
		TriggerChannel: make(chan event.GenericEvent, utils.CoalesceTriggerChannelBufferSize),
		ControllerName: fmt.Sprintf("%s-kubeadmconfig", controllerNamePrefix),
	}
	if err = uploadKubeadmConfig.SetupWithManager(mgr); err != nil {
		return reconcile.Result{}, err
	}

	uploadKubeletConfig := &controllers.KubeadmPhase{
		GetTenantControlPlaneFunc: m.retrieveTenantControlPlane(tcpCtx, request),
		Phase: &resources.KubeadmPhase{
			Client: m.AdminClient,
			Phase:  resources.PhaseUploadConfigKubelet,
		},
		TriggerChannel: make(chan event.GenericEvent, utils.CoalesceTriggerChannelBufferSize),
		ControllerName: fmt.Sprintf("%s-kubeletconfig", controllerNamePrefix),
	}
	if err = uploadKubeletConfig.SetupWithManager(mgr); err != nil {
		return reconcile.Result{}, err
	}

	bootstrapToken := &controllers.KubeadmPhase{
		GetTenantControlPlaneFunc: m.retrieveTenantControlPlane(tcpCtx, request),
		Phase: &resources.KubeadmPhase{
			Client: m.AdminClient,
			Phase:  resources.PhaseBootstrapToken,
		},
		TriggerChannel: make(chan event.GenericEvent, utils.CoalesceTriggerChannelBufferSize),
		ControllerName: fmt.Sprintf("%s-bootstraptoken", controllerNamePrefix),
	}
	if err = bootstrapToken.SetupWithManager(mgr); err != nil {
		return reconcile.Result{}, err
	}

	kubeadmRbac := &controllers.KubeadmPhase{
		GetTenantControlPlaneFunc: m.retrieveTenantControlPlane(tcpCtx, request),
		Phase: &resources.KubeadmPhase{
			Client: m.AdminClient,
			Phase:  resources.PhaseClusterAdminRBAC,
		},
		TriggerChannel: make(chan event.GenericEvent, utils.CoalesceTriggerChannelBufferSize),
		ControllerName: fmt.Sprintf("%s-kubeadmrbac", controllerNamePrefix),
	}
	if err = kubeadmRbac.SetupWithManager(mgr); err != nil {
		return reconcile.Result{}, err
	}
	completedCh := make(chan struct{})
	// Starting the manager
	go func() {
		// startErr is goroutine-local on purpose: Reconcile's named return
		// err must not be shared with this goroutine (data race, and a fast
		// Start failure could be overwritten by the synchronous return).
		if startErr := mgr.Start(tcpCtx); startErr != nil {
			logger.Error(startErr, "unable to start soot manager")
			// The sootManagerAnnotation is used to propagate the error between reconciliations with its state:
			// this is required to avoid mutex and prevent concurrent read/write on the soot map
			annotationErr := m.retryTenantControlPlaneAnnotations(ctx, request, func(annotations map[string]string) {
				annotations[sootManagerAnnotation] = sootManagerFailedAnnotation
			})
			if annotationErr != nil {
				logger.Error(annotationErr, "unable to update TenantControlPlane for soot failed annotation")
			}
			// When the manager cannot start we're enqueuing back the request to take advantage of the backoff factor
			// of the queue: this is a goroutine and cannot return an error since the manager is running on its own,
			// using the sootManagerErrChan channel we can trigger a reconciliation although the TCP hadn't any change.
			var shrunkTCP kamajiv1alpha1.TenantControlPlane

			shrunkTCP.Name = tcp.Name
			shrunkTCP.Namespace = tcp.Namespace

			m.sootManagerErrChan <- event.GenericEvent{Object: &shrunkTCP}
		}
		close(completedCh)
	}()

	// Health watchdog: probes the tenant API every
	// watchdogProbeInterval. On watchdogMaxFailures consecutive
	// failures, annotates the TCP and cancels tcpCtx, forcing the
	// soot manager goroutine above to exit and the next reconcile
	// to rebuild. Without this, controller-runtime's source.Kind
	// informers retry against an unreachable tenant API forever and
	// the only TCP-status transitions that would normally trigger
	// cleanup (VersionNotReady, VersionSleeping) never fire because
	// they depend on a working tenant connection themselves.
	go m.tenantHealthWatchdog(tcpCtx, request, tcpRest, tcpCancelFn)

	m.sootMap[request.NamespacedName.String()] = sootItem{
		certificateSha: tcp.Status.KubeConfig.Admin.Checksum,
		triggers: []chan event.GenericEvent{
			writePermissions.TriggerChannel,
			migrate.TriggerChannel,
			konnectivityAgent.TriggerChannel,
			kubeProxy.TriggerChannel,
			coreDNS.TriggerChannel,
			uploadKubeadmConfig.TriggerChannel,
			uploadKubeletConfig.TriggerChannel,
			bootstrapToken.TriggerChannel,
			kubeadmRbac.TriggerChannel,
		},
		cancelFn:            tcpCancelFn,
		completedCh:         completedCh,
		endpointObservation: endpointObservation,
	}

	return reconcile.Result{RequeueAfter: time.Second}, nil
}

func tenantEndpointObservation(tcp *kamajiv1alpha1.TenantControlPlane) string {
	if utilities.TenantClientEndpointSelection(tcp) != utilities.TenantClientEndpointClusterIP {
		return utilities.EndpointModeExternal
	}
	if token := tcp.GetAnnotations()[utilities.TenantClientEndpointTokenAnnotation]; token != "" {
		return token
	}
	// A missing token must never equal a valid acknowledgement generated by
	// k8-manager, so retirement remains fail closed.
	return "cluster-ip-without-transition-token"
}

func (m *Manager) SetupWithManager(mgr manager.Manager) error {
	m.sootManagerErrChan = make(chan event.GenericEvent)
	m.sootMap = make(map[string]sootItem)

	return controllerruntime.NewControllerManagedBy(mgr).
		WithOptions(controller.TypedOptions[reconcile.Request]{SkipNameValidation: ptr.To(true)}).
		WatchesRawSource(source.Channel(m.sootManagerErrChan, &handler.EnqueueRequestForObject{})).
		For(&kamajiv1alpha1.TenantControlPlane{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
			obj := object.(*kamajiv1alpha1.TenantControlPlane) //nolint:forcetypeassert
			// status is required to understand if we have to start or stop the soot manager
			if obj.Status.Kubernetes.Version.Status == nil {
				return false
			}

			if *obj.Status.Kubernetes.Version.Status == kamajiv1alpha1.VersionProvisioning {
				return false
			}

			return true
		}))).
		Complete(m)
}
