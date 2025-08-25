/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubevirt

import (
	gocontext "context"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubedrain "k8s.io/kubectl/pkg/drain"
	kubevirtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/noderefutil"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/context"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/ssh"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/workloadcluster"
)

const (
	vmiDeleteGraceTimeoutDurationSeconds = 600 // 10 minutes
)

// Machine implement a service for managing the KubeVirt VM hosting a kubernetes node.
type Machine struct {
	client         client.Client
	namespace      string
	machineContext *context.MachineContext
	vmiInstance    *kubevirtv1.VirtualMachineInstance
	vmInstance     *kubevirtv1.VirtualMachine
	dataVolumes    []*cdiv1.DataVolume

	sshKeys            *ssh.ClusterNodeSshKeys
	getCommandExecutor func(string, *ssh.ClusterNodeSshKeys) ssh.VMCommandExecutor
}

// NewMachine returns a new Machine service for the given context.
func NewMachine(ctx *context.MachineContext, client client.Client, namespace string, sshKeys *ssh.ClusterNodeSshKeys) (*Machine, error) {
	machine := &Machine{
		client:             client,
		namespace:          namespace,
		machineContext:     ctx,
		vmiInstance:        nil,
		vmInstance:         nil,
		sshKeys:            sshKeys,
		dataVolumes:        nil,
		getCommandExecutor: ssh.NewVMCommandExecutor,
	}

	namespacedName := types.NamespacedName{Namespace: namespace, Name: ctx.KubevirtMachine.Name}
	vm := &kubevirtv1.VirtualMachine{}
	vmi := &kubevirtv1.VirtualMachineInstance{}

	// Get the active running VMI if it exists
	err := client.Get(ctx.Context, namespacedName, vmi)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
	} else {
		machine.vmiInstance = vmi
	}

	// Get the top level VM object if it exists
	err = client.Get(ctx.Context, namespacedName, vm)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
	} else {
		machine.vmInstance = vm
	}

	if machine.vmInstance != nil {
		for _, dvTemp := range machine.vmInstance.Spec.DataVolumeTemplates {
			dv := &cdiv1.DataVolume{}
			err = client.Get(ctx.Context, types.NamespacedName{Name: dvTemp.ObjectMeta.Name, Namespace: namespace}, dv)
			if err != nil {
				if !apierrors.IsNotFound(err) {
					return nil, err
				}
			} else {
				machine.dataVolumes = append(machine.dataVolumes, dv)
			}
		}
	}

	return machine, nil
}

// IsTerminal Reports back if the VM is either being requested to terminate or is terminated
// in a way that it will never recover from.
func (m *Machine) IsTerminal() (bool, string, error) {
	if m.vmInstance == nil || m.vmiInstance == nil {
		// vm/vmi hasn't been created yet
		return false, "", nil
	}

	// VMI is being asked to terminate gracefully due to node drain
	if !m.vmiInstance.IsFinal() &&
		!m.vmiInstance.IsMigratable() &&
		m.vmiInstance.Status.EvacuationNodeName != "" {
		// VM's infra node is being drained and VM is not live migratable.
		// We need to report a FailureReason so the MachineHealthCheck and
		// MachineSet controllers will gracefully take the VM down.
		return true, "The Machine's VM pod is marked for eviction due to infra node drain.", nil
	}

	// The infrav1.KubevirtVMTerminalLabel is a way users or automation to mark
	// a VM as being in a terminal state that requires remediation. This is used
	// by the functional test suite to test remediation and can also be triggered
	// by users as a way to manually trigger remediation.
	terminalReason, ok := m.vmInstance.Labels[infrav1.KubevirtMachineVMTerminalLabel]
	if ok {
		return true, fmt.Sprintf("VM's %s label has the vm marked as being terminal with reason [%s]", infrav1.KubevirtMachineVMTerminalLabel, terminalReason), nil
	}

	// Also check the VMI for this label
	terminalReason, ok = m.vmiInstance.Labels[infrav1.KubevirtMachineVMTerminalLabel]
	if ok {
		return true, fmt.Sprintf("VMI's %s label has the vm marked as being terminal with reason [%s]", infrav1.KubevirtMachineVMTerminalLabel, terminalReason), nil
	}

	runStrategy, err := m.vmInstance.RunStrategy()
	if err != nil {
		return false, "", err
	}

	switch runStrategy {
	case kubevirtv1.RunStrategyAlways:
		// VM should recover if it is down.
		return false, "", nil
	case kubevirtv1.RunStrategyManual:
		// If VM is manually controlled, we stay out of the loop
		return false, "", nil
	case kubevirtv1.RunStrategyHalted, kubevirtv1.RunStrategyOnce:
		if m.vmiInstance.IsFinal() {
			return true, "VMI has reached a permanent finalized state", nil
		}
		return false, "", nil
	case kubevirtv1.RunStrategyRerunOnFailure:
		// only recovers when vmi is failed
		if m.vmiInstance.Status.Phase == kubevirtv1.Succeeded {
			return true, "VMI has reached a permanent finalized state", nil
		}
		return false, "", nil
	}

	return false, "", nil
}

// Exists checks if the VM has been provisioned already.
func (m *Machine) Exists() bool {
	// Check if VM was adopted first
	if m.IsAdoptedVM() {
		return m.vmInstance != nil
	}
	// Original logic for created VMs
	return m.vmInstance != nil
}

// Create creates a new VM for this machine.
func (m *Machine) Create(ctx gocontext.Context) error {
	m.machineContext.Logger.Info(fmt.Sprintf("Creating VM with role '%s'...", nodeRole(m.machineContext)))

	virtualMachine := newVirtualMachineFromKubevirtMachine(m.machineContext, m.namespace)

	mutateFn := func() (err error) {
		if virtualMachine.Labels == nil {
			virtualMachine.Labels = map[string]string{}
		}
		if virtualMachine.Spec.Template.ObjectMeta.Labels == nil {
			virtualMachine.Spec.Template.ObjectMeta.Labels = map[string]string{}
		}
		virtualMachine.Labels[clusterv1.ClusterNameLabel] = m.machineContext.Cluster.Name

		virtualMachine.Labels[infrav1.KubevirtMachineNameLabel] = m.machineContext.KubevirtMachine.Name
		virtualMachine.Labels[infrav1.KubevirtMachineNamespaceLabel] = m.machineContext.KubevirtMachine.Namespace

		virtualMachine.Spec.Template.ObjectMeta.Labels[infrav1.KubevirtMachineNameLabel] = m.machineContext.KubevirtMachine.Name
		virtualMachine.Spec.Template.ObjectMeta.Labels[infrav1.KubevirtMachineNamespaceLabel] = m.machineContext.KubevirtMachine.Namespace
		return nil
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, m.client, virtualMachine, mutateFn); err != nil {
		return err
	}

	return nil
}

// Returns if VMI has ready condition or not.
func (m *Machine) hasReadyCondition() bool {

	if m.vmiInstance == nil {
		return false
	}

	for _, cond := range m.vmiInstance.Status.Conditions {
		if cond.Type == kubevirtv1.VirtualMachineInstanceReady &&
			cond.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

// Address returns the IP address of the VM.
func (m *Machine) Address() string {
	if m.vmiInstance != nil && len(m.vmiInstance.Status.Interfaces) > 0 {
		return m.vmiInstance.Status.Interfaces[0].IP
	}

	return ""
}

// IsReady checks if the VM is ready
func (m *Machine) IsReady() bool {
	return m.hasReadyCondition()
}

// IsLiveMigratable reports back the live-migratability state of the VM: Status, Reason and Message
func (m *Machine) IsLiveMigratable() (bool, string, string, error) {
	if m.vmiInstance == nil {
		return false, "", "", fmt.Errorf("VMI is nil")
	}

	for _, cond := range m.vmiInstance.Status.Conditions {
		if cond.Type == kubevirtv1.VirtualMachineInstanceIsMigratable {
			if cond.Status == corev1.ConditionTrue {
				return true, "", "", nil
			} else {
				return false, cond.Reason, cond.Message, nil
			}
		}
	}

	return false, "", "", fmt.Errorf("%s VMI does not have a %s condition",
		m.vmiInstance.Status.Phase, kubevirtv1.VirtualMachineInstanceIsMigratable)
}

const (
	defaultCondReason  = "VMNotReady"
	defaultCondMessage = "VM is not ready"
)

func (m *Machine) GetVMNotReadyReason() (reason string, message string) {
	reason = defaultCondReason

	if m.vmInstance == nil {
		message = defaultCondMessage
		return
	}

	message = fmt.Sprintf("%s: %s", defaultCondMessage, m.vmInstance.Status.PrintableStatus)

	cond := m.getVMCondition(kubevirtv1.VirtualMachineConditionType(corev1.PodScheduled))
	if cond != nil {
		if cond.Status == corev1.ConditionTrue {
			return
		} else if cond.Status == corev1.ConditionFalse {
			if cond.Reason == "Unschedulable" {
				return "Unschedulable", cond.Message
			}
		}
	}

	for _, dv := range m.dataVolumes {
		dvReason, dvMessage, foundDVReason := m.getDVNotProvisionedReason(dv)
		if foundDVReason {
			return dvReason, dvMessage
		}
	}

	return
}

func (m *Machine) getDVNotProvisionedReason(dv *cdiv1.DataVolume) (string, string, bool) {
	msg := fmt.Sprintf("DataVolume %s is not ready; Phase: %s", dv.Name, dv.Status.Phase)
	switch dv.Status.Phase {
	case cdiv1.Succeeded: // DV's OK, return default reason & message
		return "", "", false
	case cdiv1.Pending:
		return "DVPending", msg, true
	case cdiv1.Failed:
		return "DVFailed", msg, true
	default:
		reason := "DVNotReady"
		for _, dvCond := range dv.Status.Conditions {
			if dvCond.Type == cdiv1.DataVolumeRunning {
				if dvCond.Status == corev1.ConditionFalse {
					if dvCond.Reason == "ImagePullFailed" {
						reason = "DVImagePullFailed"
					}

					msg = fmt.Sprintf("DataVolume %s import is not running: %s", dv.Name, dvCond.Message)
				}
				break
			}
		}
		return reason, msg, true
	}
}

func (m *Machine) getVMCondition(t kubevirtv1.VirtualMachineConditionType) *kubevirtv1.VirtualMachineCondition {
	if m.vmInstance == nil {
		return nil
	}

	for _, cond := range m.vmInstance.Status.Conditions {
		if cond.Type == t {
			return cond.DeepCopy()
		}
	}

	return nil
}

// SupportsCheckingIsBootstrapped checks if we have a method of checking
// that this bootstrapper has completed.
func (m *Machine) SupportsCheckingIsBootstrapped() bool {
	// Right now, we can only check if bootstrapping has
	// completed if we are using a bootstrapper that allows
	// for us to inject ssh keys into the guest.

	if m.sshKeys != nil {
		return m.machineContext.HasInjectedCapkSSHKeys(m.sshKeys.PublicKey)
	}
	return false
}

// IsBootstrapped checks if the VM is bootstrapped with Kubernetes.
func (m *Machine) IsBootstrapped() bool {
	// CheckStrategy value is already sanitized by apiserver
	switch m.machineContext.KubevirtMachine.Spec.BootstrapCheckSpec.CheckStrategy {
	case "none":
		// skip bootstrap check and always returns positively
		return true

	case "":
		fallthrough // ssh is default check strategy, fallthrough
	case "ssh":
		return m.IsBootstrappedWithSSH()

	default:
		// Since CRD CheckStrategy field is validated by an enum, this case should never be hit
		return false
	}
}

// IsBootstrappedWithSSH checks if the VM is bootstrapped with Kubernetes using SSH strategy.
func (m *Machine) IsBootstrappedWithSSH() bool {
	if !m.IsReady() || m.sshKeys == nil {
		return false
	}

	executor := m.getCommandExecutor(m.Address(), m.sshKeys)

	output, err := executor.ExecuteCommand("cat /run/cluster-api/bootstrap-success.complete")
	if err != nil || output != "success" {
		return false
	}
	return true
}

// GenerateProviderID generates the KubeVirt provider ID to be used for the NodeRef
func (m *Machine) GenerateProviderID() (string, error) {
	if m.vmiInstance == nil {
		return "", errors.New("Underlying Kubevirt VM is NOT running")
	}

	providerID := fmt.Sprintf("kubevirt://%s", m.machineContext.KubevirtMachine.Name)

	return providerID, nil
}

// Delete deletes VM for this machine.
func (m *Machine) Delete() error {
	namespacedName := types.NamespacedName{Namespace: m.namespace, Name: m.machineContext.KubevirtMachine.Name}
	vm := &kubevirtv1.VirtualMachine{}
	if err := m.client.Get(m.machineContext.Context, namespacedName, vm); err != nil {
		if apierrors.IsNotFound(err) {
			m.machineContext.Logger.Info("VM does not exist, nothing to do.")
			return nil
		}
		return errors.Wrapf(err, "failed to retrieve VM to delete")
	}

	if err := m.client.Delete(gocontext.Background(), vm); err != nil {
		return errors.Wrapf(err, "failed to delete VM")
	}

	return nil
}

func (m *Machine) DrainNodeIfNeeded(wrkldClstr workloadcluster.WorkloadCluster) (time.Duration, error) {
	if m.vmiInstance == nil || !m.shouldGracefulDeleteVMI() {
		if _, anntExists := m.machineContext.KubevirtMachine.Annotations[infrav1.VmiDeletionGraceTime]; anntExists {
			if err := m.removeGracePeriodAnnotation(); err != nil {
				return 100 * time.Millisecond, err
			}
		}
		return 0, nil
	}

	exceeded, err := m.drainGracePeriodExceeded()
	if err != nil {
		return 0, err
	}

	if !exceeded {
		retryDuration, err := m.drainNode(wrkldClstr)
		if err != nil {
			return 0, err
		}

		if retryDuration > 0 {
			return retryDuration, nil
		}
	}

	// now, when the node is drained (or vmiDeleteGraceTimeoutDurationSeconds has passed), we can delete the VMI
	propagationPolicy := metav1.DeletePropagationForeground
	err = m.client.Delete(m.machineContext, m.vmiInstance, &client.DeleteOptions{PropagationPolicy: &propagationPolicy})
	if err != nil {
		m.machineContext.Logger.Error(err, "failed to delete VirtualMachineInstance")
		return 0, err
	}

	if err = m.removeGracePeriodAnnotation(); err != nil {
		return 100 * time.Millisecond, err
	}

	// requeue to force reading the VMI again
	return time.Second * 10, nil
}

const removeGracePeriodAnnotationPatch = `[{"op": "remove", "path": "/metadata/annotations/` + infrav1.VmiDeletionGraceTimeEscape + `"}]`

func (m *Machine) removeGracePeriodAnnotation() error {
	patch := client.RawPatch(types.JSONPatchType, []byte(removeGracePeriodAnnotationPatch))

	if err := m.client.Patch(m.machineContext, m.machineContext.KubevirtMachine, patch); err != nil {
		return fmt.Errorf("failed to remove the %s annotation to the KubeVirtMachine %s; %w", infrav1.VmiDeletionGraceTime, m.machineContext.KubevirtMachine.Name, err)
	}

	return nil
}

func (m *Machine) shouldGracefulDeleteVMI() bool {
	if m.vmiInstance.DeletionTimestamp != nil {
		m.machineContext.Logger.V(4).Info("DrainNode: the virtualMachineInstance is already in deletion process. Nothing to do here")
		return false
	}

	if m.vmiInstance.Spec.EvictionStrategy == nil || *m.vmiInstance.Spec.EvictionStrategy != kubevirtv1.EvictionStrategyExternal {
		m.machineContext.Logger.V(4).Info("DrainNode: graceful deletion is not supported for virtualMachineInstance. Nothing to do here")
		return false
	}

	// KubeVirt will set the EvacuationNodeName field in case of guest node eviction. If the field is not set, there is
	// nothing to do.
	if len(m.vmiInstance.Status.EvacuationNodeName) == 0 {
		m.machineContext.Logger.V(4).Info("DrainNode: the virtualMachineInstance is not marked for deletion. Nothing to do here")
		return false
	}

	return true
}

// wait vmiDeleteGraceTimeoutDurationSeconds to the node to be drained. If this time had passed, don't wait anymore.
func (m *Machine) drainGracePeriodExceeded() (bool, error) {
	if graceTime, found := m.machineContext.KubevirtMachine.Annotations[infrav1.VmiDeletionGraceTime]; found {
		deletionGraceTime, err := time.Parse(time.RFC3339, graceTime)
		if err != nil { // wrong format - rewrite
			if err = m.setVmiDeletionGraceTime(); err != nil {
				return false, err
			}
		} else {
			return time.Now().UTC().After(deletionGraceTime), nil
		}
	} else {
		if err := m.setVmiDeletionGraceTime(); err != nil {
			return false, err
		}
	}

	return false, nil
}

func (m *Machine) setVmiDeletionGraceTime() error {
	m.machineContext.Logger.Info(fmt.Sprintf("setting the %s annotation", infrav1.VmiDeletionGraceTime))
	graceTime := time.Now().Add(vmiDeleteGraceTimeoutDurationSeconds * time.Second).UTC().Format(time.RFC3339)
	patch := fmt.Sprintf(`{"metadata":{"annotations":{"%s": "%s"}}}`, infrav1.VmiDeletionGraceTime, graceTime)
	patchRequest := client.RawPatch(types.MergePatchType, []byte(patch))

	if err := m.client.Patch(m.machineContext, m.machineContext.KubevirtMachine, patchRequest); err != nil {
		return fmt.Errorf("failed to add the %s annotation to the KubeVirtMachine %s; %w", infrav1.VmiDeletionGraceTime, m.machineContext.KubevirtMachine.Name, err)
	}

	return nil
}

// This functions drains a node from a tenant cluster.
// The function returns 3 values:
// * drain done - boolean
// * retry time, or 0 if not needed
// * error - to be returned if we want to retry
func (m *Machine) drainNode(wrkldClstr workloadcluster.WorkloadCluster) (time.Duration, error) {
	kubeClient, err := wrkldClstr.GenerateWorkloadClusterK8sClient(m.machineContext)
	if err != nil {
		m.machineContext.Logger.Error(err, "Error creating a remote client while deleting Machine, won't retry")
		return 0, fmt.Errorf("failed to get client to remote cluster; %w", err)
	}

	nodeName := m.vmiInstance.Status.EvacuationNodeName
	node, err := kubeClient.CoreV1().Nodes().Get(m.machineContext, nodeName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If an admin deletes the node directly, we'll end up here.
			m.machineContext.Logger.Error(err, "Could not find node from noderef, it may have already been deleted")
			return 0, nil
		}
		return 0, fmt.Errorf("unable to get node %q: %w", nodeName, err)
	}

	drainer := &kubedrain.Helper{
		Client:              kubeClient,
		Ctx:                 m.machineContext,
		Force:               true,
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  true,
		GracePeriodSeconds:  -1,
		// If a pod is not evicted in 20 seconds, retry the eviction next time the
		// machine gets reconciled again (to allow other machines to be reconciled).
		Timeout: 20 * time.Second,
		OnPodDeletedOrEvicted: func(pod *corev1.Pod, usingEviction bool) {
			verbStr := "Deleted"
			if usingEviction {
				verbStr = "Evicted"
			}
			m.machineContext.Logger.Info(fmt.Sprintf("%s pod from Node", verbStr),
				"pod", fmt.Sprintf("%s/%s", pod.Name, pod.Namespace))
		},
		Out: writer{m.machineContext.Logger.Info},
		ErrOut: writer{func(msg string, keysAndValues ...interface{}) {
			m.machineContext.Logger.Error(nil, msg, keysAndValues...)
		}},
	}

	if noderefutil.IsNodeUnreachable(node) {
		// When the node is unreachable and some pods are not evicted for as long as this timeout, we ignore them.
		drainer.SkipWaitForDeleteTimeoutSeconds = 60 * 5 // 5 minutes
	}

	if err = kubedrain.RunCordonOrUncordon(drainer, node, true); err != nil {
		// Machine will be re-reconciled after a cordon failure.
		m.machineContext.Logger.Error(err, "Cordon failed")
		return 0, errors.Errorf("unable to cordon node %s: %v", nodeName, err)
	}

	if err = kubedrain.RunNodeDrain(drainer, node.Name); err != nil {
		// Machine will be re-reconciled after a drain failure.
		m.machineContext.Logger.Error(err, "Drain failed, retry in a second", "node name", nodeName)
		return time.Second, nil
	}

	m.machineContext.Logger.Info("Drain successful", "node name", nodeName)
	return 0, nil
}

// writer implements io.Writer interface as a pass-through for klog.
type writer struct {
	logFunc func(msg string, keysAndValues ...interface{})
}

// Write passes string(p) into writer's logFunc and always returns len(p).
func (w writer) Write(p []byte) (n int, err error) {
	w.logFunc(string(p))
	return len(p), nil
}

// getAdoptionPhase returns the current adoption phase from machine annotations
func (m *Machine) getAdoptionPhase() string {
	if m.machineContext.KubevirtMachine.Annotations == nil {
		return infrav1.AdoptionPhaseNone
	}
	phase, exists := m.machineContext.KubevirtMachine.Annotations[infrav1.AdoptionPhase]
	if !exists {
		return infrav1.AdoptionPhaseNone
	}
	return phase
}

// setAdoptionPhase sets the adoption phase annotation
func (m *Machine) setAdoptionPhase(phase string) error {
	if m.machineContext.KubevirtMachine.Annotations == nil {
		m.machineContext.KubevirtMachine.Annotations = make(map[string]string)
	}
	m.machineContext.KubevirtMachine.Annotations[infrav1.AdoptionPhase] = phase
	return nil
}

// hasExistingVMAnnotation checks if the existing VM annotation is present
func (m *Machine) hasExistingVMAnnotation() bool {
	if m.machineContext.KubevirtMachine.Annotations == nil {
		return false
	}
	existingVMName, exists := m.machineContext.KubevirtMachine.Annotations[infrav1.ExistingVMName]
	return exists && existingVMName != ""
}

// getExistingVMName returns the existing VM name from annotation
func (m *Machine) getExistingVMName() string {
	if m.machineContext.KubevirtMachine.Annotations == nil {
		return ""
	}
	return m.machineContext.KubevirtMachine.Annotations[infrav1.ExistingVMName]
}

// NeedsAdoption checks if VM needs to start adoption process
func (m *Machine) NeedsAdoption() bool {
	phase := m.getAdoptionPhase()
	return phase == infrav1.AdoptionPhaseNone && m.hasExistingVMAnnotation()
}

// IsAdoptionInProgress checks if adoption is currently in progress
func (m *Machine) IsAdoptionInProgress() bool {
	phase := m.getAdoptionPhase()
	return phase != infrav1.AdoptionPhaseNone && phase != infrav1.AdoptionPhaseCompleted
}

// IsAdoptionFailed checks if adoption has failed
func (m *Machine) IsAdoptionFailed() bool {
	return m.getAdoptionPhase() == infrav1.AdoptionPhaseFailed
}

// GetAdoptionPhase returns the current adoption phase (interface method)
func (m *Machine) GetAdoptionPhase() string {
	return m.getAdoptionPhase()
}

// SetAdoptionPhase sets the adoption phase (interface method)
func (m *Machine) SetAdoptionPhase(phase string) error {
	return m.setAdoptionPhase(phase)
}

// isBootstrapCompleted checks if bootstrap has actually completed successfully
func (m *Machine) isBootstrapCompleted() bool {
	// Check CAPI bootstrap success condition
	return conditions.IsTrue(m.machineContext.KubevirtMachine, infrav1.BootstrapExecSucceededCondition)
}

// IsAdoptedVM checks if this machine represents a fully adopted existing VM
// Returns true only if adoption process is completely finished with bootstrap success
func (m *Machine) IsAdoptedVM() bool {
	logger := m.machineContext.Logger.WithValues("component", "adoption-check")
	
	// Must have existing VM annotation to be considered adopted
	if !m.hasExistingVMAnnotation() {
		logger.V(2).Info("No existing-vm-name annotation found")
		return false
	}
	
	existingVMName := m.getExistingVMName()
	phase := m.getAdoptionPhase()
	
	logger.Info("Checking adoption status", 
		"vmName", existingVMName, 
		"phase", phase,
		"bootstrapCompleted", m.isBootstrapCompleted())
	
	// Only consider fully adopted if:
	// 1. Adoption phase is completed AND
	// 2. Bootstrap has actually succeeded
	if phase == infrav1.AdoptionPhaseCompleted && m.isBootstrapCompleted() {
		logger.Info("VM is fully adopted - phase completed and bootstrap succeeded", 
			"vm", existingVMName)
		return true
	}
	
	// If we have labels but bootstrap not completed, we need to continue adoption
	if phase == infrav1.AdoptionPhaseLabelsApplied || phase == infrav1.AdoptionPhaseBootstrapping {
		logger.Info("VM adoption in progress - labels applied but bootstrap not completed", 
			"vm", existingVMName, "phase", phase)
		return false
	}
	
	// For any other state, not adopted
	logger.Info("VM not fully adopted", "vm", existingVMName, "phase", phase)
	return false
}

// AdoptExistingVM adopts an existing VM into CAPI management with phase tracking
func (m *Machine) AdoptExistingVM(ctx gocontext.Context, vmName string) error {
	logger := m.machineContext.Logger.WithValues("vm", vmName, "phase", "adoption")
	logger.Info("Starting VM adoption process")

	// Mark adoption as detected
	if err := m.setAdoptionPhase(infrav1.AdoptionPhaseDetected); err != nil {
		return errors.Wrapf(err, "failed to set adoption phase to detected")
	}

	// 1. Find existing VM
	vm := &kubevirtv1.VirtualMachine{}
	namespacedName := types.NamespacedName{Namespace: m.namespace, Name: vmName}
	if err := m.client.Get(ctx, namespacedName, vm); err != nil {
		m.setAdoptionPhase(infrav1.AdoptionPhaseFailed)
		return errors.Wrapf(err, "failed to find existing VM: %s", vmName)
	}
	logger.Info("Found existing VM", "vmState", vm.Status.PrintableStatus)

	// 2. Validate VM compatibility
	if err := m.validateVMForAdoption(vm); err != nil {
		m.setAdoptionPhase(infrav1.AdoptionPhaseFailed)
		return errors.Wrapf(err, "VM validation failed")
	}
	logger.Info("VM validation passed")

	// 3. Apply CAPI management labels
	if err := m.applyCAPILabels(ctx, vm); err != nil {
		m.setAdoptionPhase(infrav1.AdoptionPhaseFailed)
		return errors.Wrapf(err, "failed to apply CAPI labels")
	}
	logger.Info("CAPI labels applied successfully")

	// Mark labels as applied
	if err := m.setAdoptionPhase(infrav1.AdoptionPhaseLabelsApplied); err != nil {
		return errors.Wrapf(err, "failed to set adoption phase to labels-applied")
	}

	// 4. Update machine context
	m.vmInstance = vm

	// 5. Try to get VMI as well
	vmi := &kubevirtv1.VirtualMachineInstance{}
	if err := m.client.Get(ctx, namespacedName, vmi); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "Failed to get VMI for adopted VM - continuing without VMI")
		} else {
			logger.Info("VMI not found - VM might not be running yet")
		}
		// VMI not found is OK - VM might not be running yet
	} else {
		m.vmiInstance = vmi
		logger.Info("Found VMI for adopted VM", "vmiPhase", vmi.Status.Phase)
	}

	logger.Info("VM adoption completed successfully - ready for bootstrap injection")
	return nil
}

// validateVMForAdoption validates that an existing VM can be adopted
func (m *Machine) validateVMForAdoption(vm *kubevirtv1.VirtualMachine) error {
	// VM can be in any state - we'll restart it for bootstrap injection
	m.machineContext.Logger.Info(fmt.Sprintf("VM current state: %s - will restart for bootstrap injection", vm.Status.PrintableStatus))
	
	// Validate VM has required volumes for cloud-init injection
	hasCloudInit := false
	for _, volume := range vm.Spec.Template.Spec.Volumes {
		if volume.Name == "cloudinit" && volume.CloudInitNoCloud != nil {
			hasCloudInit = true
			break
		}
	}
	if !hasCloudInit {
		return fmt.Errorf("VM must have cloudinit volume for bootstrap injection")
	}

	// No guest agent required - using restart approach for bootstrap injection
	m.machineContext.Logger.Info("VM validation passed - restart-based adoption will be used")
	return nil
}

// applyCAPILabels applies necessary CAPI management labels to the adopted VM
func (m *Machine) applyCAPILabels(ctx gocontext.Context, vm *kubevirtv1.VirtualMachine) error {
	if vm.Labels == nil {
		vm.Labels = make(map[string]string)
	}

	// Apply CAPI management labels
	vm.Labels[clusterv1.ClusterNameLabel] = m.machineContext.Cluster.Name
	vm.Labels[infrav1.KubevirtMachineNameLabel] = m.machineContext.KubevirtMachine.Name
	vm.Labels[infrav1.KubevirtMachineNamespaceLabel] = m.machineContext.KubevirtMachine.Namespace
	vm.Labels["cluster.x-k8s.io/role"] = nodeRole(m.machineContext)

	// Also apply labels to VMI template for consistency
	if vm.Spec.Template.ObjectMeta.Labels == nil {
		vm.Spec.Template.ObjectMeta.Labels = make(map[string]string)
	}
	vm.Spec.Template.ObjectMeta.Labels[infrav1.KubevirtMachineNameLabel] = m.machineContext.KubevirtMachine.Name
	vm.Spec.Template.ObjectMeta.Labels[infrav1.KubevirtMachineNamespaceLabel] = m.machineContext.KubevirtMachine.Namespace

	// Update VM
	if err := m.client.Update(ctx, vm); err != nil {
		return errors.Wrapf(err, "failed to update VM labels")
	}

	return nil
}

// RestartVMWithBootstrap stops and starts VM to inject bootstrap data with enhanced validation and phase tracking
func (m *Machine) RestartVMWithBootstrap(ctx gocontext.Context) error {
	// Get VM directly instead of relying on m.vmInstance field
	vmName := m.getExistingVMName()
	if vmName == "" {
		return fmt.Errorf("no existing VM name configured for bootstrap injection")
	}
	
	// Fetch VM object
	vm := &kubevirtv1.VirtualMachine{}
	namespacedName := types.NamespacedName{
		Name:      vmName,
		Namespace: m.namespace,
	}
	if err := m.client.Get(ctx, namespacedName, vm); err != nil {
		return fmt.Errorf("failed to get VM %s for bootstrap injection: %w", vmName, err)
	}
	
	logger := m.machineContext.Logger.WithValues("vm", vmName, "phase", "bootstrap-restart")
	
	logger.Info("=== Starting simplified VM bootstrap injection ===")

	// Mark bootstrap injection as starting
	if err := m.setAdoptionPhase(infrav1.AdoptionPhaseBootstrapping); err != nil {
		return fmt.Errorf("failed to set adoption phase to bootstrapping: %w", err)
	}

	// Get bootstrap data
	bootstrapData, err := m.getBootstrapDataFromSecret(ctx)
	if err != nil {
		m.setAdoptionPhase(infrav1.AdoptionPhaseFailed)
		return fmt.Errorf("failed to get bootstrap data: %w", err)
	}
	logger.Info("Bootstrap data retrieved", "size", len(bootstrapData))

	// CORE LOGIC: Inject bootstrap data directly into VM cloud-init
	// This is the CORRECT approach - inject to VM.spec, not VMI
	logger.Info("Injecting bootstrap data into VM cloud-init spec")
	if err := m.injectBootstrapDataToVMWithValidation(ctx, vm, bootstrapData); err != nil {
		m.setAdoptionPhase(infrav1.AdoptionPhaseFailed)
		return fmt.Errorf("failed to inject bootstrap data: %w", err)
	}
	logger.Info("Bootstrap data injection completed successfully")

	// Simple restart: stop → start VM to pick up new cloud-init
	logger.Info("Restarting VM to apply new cloud-init configuration")
	if err := m.simpleVMRestart(ctx, vm); err != nil {
		m.setAdoptionPhase(infrav1.AdoptionPhaseFailed)
		return fmt.Errorf("failed to restart VM: %w", err)
	}
	
	logger.Info("=== VM bootstrap injection completed successfully ===")
	return nil
}

// simpleVMRestart performs a simple VM restart: stop → start to apply new cloud-init
func (m *Machine) simpleVMRestart(ctx gocontext.Context, vm *kubevirtv1.VirtualMachine) error {
	logger := m.machineContext.Logger.WithValues("vm", vm.Name, "action", "simple-restart")
	
	// Refresh VM to get current status
	namespacedName := types.NamespacedName{Name: vm.Name, Namespace: vm.Namespace}
	if err := m.client.Get(ctx, namespacedName, vm); err != nil {
		return fmt.Errorf("failed to refresh VM status: %w", err)
	}
	
	// Stop VM if running
	if vm.Status.PrintableStatus == kubevirtv1.VirtualMachineStatusRunning {
		logger.Info("Stopping VM for cloud-init update", "currentStatus", vm.Status.PrintableStatus)
		vm.Spec.Running = &[]bool{false}[0]
		if err := m.client.Update(ctx, vm); err != nil {
			return fmt.Errorf("failed to stop VM: %w", err)
		}
		
		// Wait for VM to stop (simple wait, no complex retry)
		logger.Info("Waiting for VM to stop...")
		time.Sleep(15 * time.Second)
	} else {
		logger.Info("VM not running, proceeding with start", "currentStatus", vm.Status.PrintableStatus)
	}
	
	// Refresh VM again before starting
	if err := m.client.Get(ctx, namespacedName, vm); err != nil {
		return fmt.Errorf("failed to refresh VM status before start: %w", err)
	}
	
	// Start VM with new cloud-init
	logger.Info("Starting VM with new cloud-init configuration")
	vm.Spec.Running = &[]bool{true}[0]
	if err := m.client.Update(ctx, vm); err != nil {
		return fmt.Errorf("failed to start VM: %w", err)
	}
	
	logger.Info("VM restart completed - new cloud-init will be applied on next boot")
	return nil
}

// getBootstrapDataFromSecret extracts and validates bootstrap data from the CAPI bootstrap secret
func (m *Machine) getBootstrapDataFromSecret(ctx gocontext.Context) (string, error) {
	logger := m.machineContext.Logger.WithValues("component", "bootstrap-secret")
	
	if m.machineContext.Machine.Spec.Bootstrap.DataSecretName == nil {
		return "", fmt.Errorf("bootstrap data secret name is nil")
	}

	secretName := *m.machineContext.Machine.Spec.Bootstrap.DataSecretName
	logger.Info("Retrieving bootstrap secret", "secret", secretName)
	
	// Retry logic for secret retrieval
	var secret *corev1.Secret
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		secret = &corev1.Secret{}
		secretKey := client.ObjectKey{
			Namespace: m.machineContext.Machine.Namespace,
			Name:      secretName,
		}

		err = m.client.Get(ctx, secretKey, secret)
		if err == nil {
			break
		}
		
		logger.Info("Bootstrap secret not found, retrying", "attempt", attempt, "error", err.Error())
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
	}
	
	if err != nil {
		return "", errors.Wrapf(err, "failed to get bootstrap secret %s after retries", secretName)
	}

	bootstrapData, exists := secret.Data["value"]
	if !exists {
		return "", fmt.Errorf("bootstrap secret %s missing 'value' key", secretName)
	}

	// Validate bootstrap data content
	bootstrapStr := string(bootstrapData)
	if len(bootstrapStr) == 0 {
		return "", fmt.Errorf("bootstrap secret %s contains empty data", secretName)
	}
	
	// Basic validation for kubeadm bootstrap script
	if !strings.Contains(bootstrapStr, "kubeadm") {
		logger.Info("Warning: bootstrap data doesn't contain kubeadm references", "preview", bootstrapStr[:min(200, len(bootstrapStr))])
	}
	
	logger.Info("Bootstrap secret retrieved successfully", "size", len(bootstrapStr))
	return bootstrapStr, nil
}

// injectBootstrapDataToVMWithValidation updates VM cloud-init with CAPI bootstrap data and validates
func (m *Machine) injectBootstrapDataToVMWithValidation(ctx gocontext.Context, vm *kubevirtv1.VirtualMachine, bootstrapData string) error {
	logger := m.machineContext.Logger.WithValues("component", "bootstrap-injection")
	
	// KubeVirt has a 2048 byte limit for inline userData - use secret for larger data
	const kubevirtUserDataLimit = 2048
	useSecret := len(bootstrapData) > kubevirtUserDataLimit
	
	if useSecret {
		logger.Info("Bootstrap data exceeds KubeVirt inline limit, using UserDataSecretRef", 
			"dataSize", len(bootstrapData), "limit", kubevirtUserDataLimit)
		return m.injectBootstrapDataViaSecret(ctx, vm, bootstrapData)
	} else {
		logger.Info("Using inline userData for bootstrap injection", "dataSize", len(bootstrapData))
		return m.injectBootstrapDataInline(ctx, vm, bootstrapData)
	}
}

// injectBootstrapDataInline injects bootstrap data directly into VM cloud-init userData (for small data)
func (m *Machine) injectBootstrapDataInline(ctx gocontext.Context, vm *kubevirtv1.VirtualMachine, bootstrapData string) error {
	logger := m.machineContext.Logger.WithValues("component", "bootstrap-injection-inline")
	
	// Find cloud-init volume in VM spec
	for i, volume := range vm.Spec.Template.Spec.Volumes {
		if volume.Name == "cloudinit" && volume.CloudInitNoCloud != nil {
			// Store original data for rollback if needed
			originalData := volume.CloudInitNoCloud.UserData
			logger.Info("Found cloud-init volume", "originalSize", len(originalData), "newSize", len(bootstrapData))
			
			// Replace existing cloud-init with bootstrap data
			vm.Spec.Template.Spec.Volumes[i].CloudInitNoCloud.UserData = bootstrapData
			// Clear any existing UserDataSecretRef
			vm.Spec.Template.Spec.Volumes[i].CloudInitNoCloud.UserDataSecretRef = nil
			
			// Update VM spec with retry logic
			if err := m.updateVMWithRetry(ctx, vm, i, bootstrapData, false); err != nil {
				return err
			}
			
			logger.Info("Bootstrap data successfully injected into VM cloud-init inline")
			return nil
		}
	}

	return fmt.Errorf("cloud-init volume not found in VM spec")
}

// injectBootstrapDataViaSecret injects bootstrap data via UserDataSecretRef (for large data)
func (m *Machine) injectBootstrapDataViaSecret(ctx gocontext.Context, vm *kubevirtv1.VirtualMachine, bootstrapData string) error {
	logger := m.machineContext.Logger.WithValues("component", "bootstrap-injection-secret")
	
	// Create secret with bootstrap data
	secretName := fmt.Sprintf("%s-bootstrap-userdata", vm.Name)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: vm.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "cluster-api-provider-kubevirt",
				"cluster.x-k8s.io/cluster-name": m.machineContext.Cluster.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"userdata": bootstrapData,
		},
	}
	
	// Create or update the secret
	_, err := controllerutil.CreateOrUpdate(ctx, m.client, secret, func() error {
		secret.StringData = map[string]string{
			"userdata": bootstrapData,
		}
		return nil
	})
	if err != nil {
		return errors.Wrapf(err, "failed to create/update bootstrap secret %s", secretName)
	}
	logger.Info("Bootstrap secret created/updated", "secret", secretName, "dataSize", len(bootstrapData))

	// Find cloud-init volume in VM spec and update to use secret
	for i, volume := range vm.Spec.Template.Spec.Volumes {
		if volume.Name == "cloudinit" && volume.CloudInitNoCloud != nil {
			logger.Info("Found cloud-init volume, updating to use UserDataSecretRef", "secret", secretName)
			
			// Clear inline userData and set secret reference
			vm.Spec.Template.Spec.Volumes[i].CloudInitNoCloud.UserData = ""
			vm.Spec.Template.Spec.Volumes[i].CloudInitNoCloud.UserDataSecretRef = &corev1.LocalObjectReference{
				Name: secretName,
			}
			
			// Update VM spec with retry logic
			if err := m.updateVMWithRetry(ctx, vm, i, secretName, true); err != nil {
				return err
			}
			
			logger.Info("Bootstrap data successfully injected via UserDataSecretRef", "secret", secretName)
			return nil
		}
	}

	return fmt.Errorf("cloud-init volume not found in VM spec")
}

// updateVMWithRetry handles VM update with retry logic for both inline and secret-based injection
func (m *Machine) updateVMWithRetry(ctx gocontext.Context, vm *kubevirtv1.VirtualMachine, volumeIndex int, data string, isSecret bool) error {
	logger := m.machineContext.Logger.WithValues("component", "vm-update-retry")
	
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = m.client.Update(ctx, vm)
		if err == nil {
			break
		}
		
		logger.Info("Failed to update VM with bootstrap data, retrying", "attempt", attempt, "error", err.Error())
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			// Refresh VM object before retry
			namespacedName := types.NamespacedName{Namespace: vm.Namespace, Name: vm.Name}
			if refreshErr := m.client.Get(ctx, namespacedName, vm); refreshErr != nil {
				logger.Error(refreshErr, "Failed to refresh VM object for retry")
				continue
			}
			
			// Re-apply bootstrap data based on method
			if isSecret {
				vm.Spec.Template.Spec.Volumes[volumeIndex].CloudInitNoCloud.UserData = ""
				vm.Spec.Template.Spec.Volumes[volumeIndex].CloudInitNoCloud.UserDataSecretRef = &corev1.LocalObjectReference{
					Name: data, // data is secret name in this case
				}
			} else {
				vm.Spec.Template.Spec.Volumes[volumeIndex].CloudInitNoCloud.UserData = data
				vm.Spec.Template.Spec.Volumes[volumeIndex].CloudInitNoCloud.UserDataSecretRef = nil
			}
		}
	}
	
	if err != nil {
		return errors.Wrapf(err, "failed to update VM with bootstrap data after retries")
	}
	
	return nil
}

// waitForVMState waits for VM to reach the specified state within timeout with enhanced monitoring
func (m *Machine) waitForVMState(ctx gocontext.Context, vmName, targetState string, timeout time.Duration) error {
	logger := m.machineContext.Logger.WithValues("vm", vmName, "targetState", targetState)
	deadline := time.Now().Add(timeout)
	start := time.Now()
	previousState := ""
	stateChanges := 0
	
	logger.Info("Starting VM state monitoring", "timeout", timeout.String())
	
	for time.Now().Before(deadline) {
		vm := &kubevirtv1.VirtualMachine{}
		namespacedName := types.NamespacedName{Namespace: m.namespace, Name: vmName}
		
		if err := m.client.Get(ctx, namespacedName, vm); err != nil {
			return errors.Wrapf(err, "failed to get VM status")
		}

		currentState := string(vm.Status.PrintableStatus)
		elapsed := time.Since(start)
		
		// Track state changes
		if currentState != previousState {
			stateChanges++
			logger.Info("VM state changed", "from", previousState, "to", currentState, "elapsed", elapsed.String(), "changes", stateChanges)
			previousState = currentState
		}
		
		if currentState == targetState {
			logger.Info("VM reached target state", "finalState", currentState, "totalTime", elapsed.String(), "stateChanges", stateChanges)
			return nil
		}

		// Check for stuck states
		if elapsed > timeout/2 && stateChanges == 0 {
			logger.Info("Warning: VM appears stuck", "state", currentState, "elapsed", elapsed.String())
		}

		time.Sleep(5 * time.Second)
	}

	return fmt.Errorf("timeout waiting for VM %s to reach state %s (current: %s, elapsed: %s, changes: %d)", 
		vmName, targetState, previousState, time.Since(start).String(), stateChanges)
}

// validateVMForBootstrapRestart validates VM is ready for bootstrap restart process
func (m *Machine) validateVMForBootstrapRestart(ctx gocontext.Context, vm *kubevirtv1.VirtualMachine) error {
	logger := m.machineContext.Logger.WithValues("component", "pre-restart-validation")
	
	// Check VM exists and is accessible
	if vm == nil {
		return fmt.Errorf("VM object is nil")
	}
	
	// Validate cloud-init volume exists
	hasCloudInit := false
	for _, volume := range vm.Spec.Template.Spec.Volumes {
		if volume.Name == "cloudinit" && volume.CloudInitNoCloud != nil {
			hasCloudInit = true
			logger.Info("Found cloud-init volume for bootstrap injection")
			break
		}
	}
	if !hasCloudInit {
		return fmt.Errorf("VM must have cloudinit volume for bootstrap injection")
	}
	
	// Check VM is not in a problematic state
	currentState := string(vm.Status.PrintableStatus)
	if currentState == "Unknown" || currentState == "Failed" {
		return fmt.Errorf("VM in problematic state: %s", currentState)
	}
	
	logger.Info("VM pre-restart validation passed", "currentState", currentState)
	return nil
}

// stopVMWithRetry stops VM with retry logic
func (m *Machine) stopVMWithRetry(ctx gocontext.Context, vm *kubevirtv1.VirtualMachine, maxRetries int) error {
	logger := m.machineContext.Logger.WithValues("component", "vm-stop")
	
	for attempt := 1; attempt <= maxRetries; attempt++ {
		logger.Info("Attempting to stop VM", "attempt", attempt, "maxRetries", maxRetries)
		
		// Set VM to halted state
		haltedStrategy := kubevirtv1.RunStrategyHalted
		vm.Spec.RunStrategy = &haltedStrategy
		
		if err := m.client.Update(ctx, vm); err != nil {
			logger.Info("Failed to update VM to halted state", "attempt", attempt, "error", err.Error())
			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt) * 3 * time.Second)
				// Refresh VM object
				namespacedName := types.NamespacedName{Namespace: vm.Namespace, Name: vm.Name}
				if refreshErr := m.client.Get(ctx, namespacedName, vm); refreshErr != nil {
					logger.Error(refreshErr, "Failed to refresh VM object for stop retry")
					continue
				}
				continue
			}
			return errors.Wrapf(err, "failed to stop VM after %d attempts", maxRetries)
		}
		
		// Wait for VM to stop
		timeout := time.Duration(attempt) * 30 * time.Second
		if err := m.waitForVMState(ctx, vm.Name, "Stopped", timeout); err != nil {
			logger.Info("VM failed to stop within timeout", "attempt", attempt, "timeout", timeout.String())
			if attempt < maxRetries {
				continue
			}
			return errors.Wrapf(err, "VM failed to stop after %d attempts", maxRetries)
		}
		
		logger.Info("VM stopped successfully", "attempt", attempt)
		return nil
	}
	
	return fmt.Errorf("VM stop failed after %d attempts", maxRetries)
}

// startVMWithRetry starts VM with retry logic
func (m *Machine) startVMWithRetry(ctx gocontext.Context, vm *kubevirtv1.VirtualMachine, maxRetries int) error {
	logger := m.machineContext.Logger.WithValues("component", "vm-start")
	
	for attempt := 1; attempt <= maxRetries; attempt++ {
		logger.Info("Attempting to start VM", "attempt", attempt, "maxRetries", maxRetries)
		
		// Set VM to always running state
		alwaysStrategy := kubevirtv1.RunStrategyAlways
		vm.Spec.RunStrategy = &alwaysStrategy
		
		if err := m.client.Update(ctx, vm); err != nil {
			logger.Info("Failed to update VM to running state", "attempt", attempt, "error", err.Error())
			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt) * 3 * time.Second)
				// Refresh VM object
				namespacedName := types.NamespacedName{Namespace: vm.Namespace, Name: vm.Name}
				if refreshErr := m.client.Get(ctx, namespacedName, vm); refreshErr != nil {
					logger.Error(refreshErr, "Failed to refresh VM object for start retry")
					continue
				}
				continue
			}
			return errors.Wrapf(err, "failed to start VM after %d attempts", maxRetries)
		}
		
		// Wait for VM to start
		timeout := time.Duration(attempt) * 60 * time.Second
		if err := m.waitForVMState(ctx, vm.Name, "Running", timeout); err != nil {
			logger.Info("VM failed to start within timeout", "attempt", attempt, "timeout", timeout.String())
			if attempt < maxRetries {
				continue
			}
			return errors.Wrapf(err, "VM failed to start after %d attempts", maxRetries)
		}
		
		logger.Info("VM started successfully", "attempt", attempt)
		return nil
	}
	
	return fmt.Errorf("VM start failed after %d attempts", maxRetries)
}

// verifyBootstrapInjection validates that bootstrap data was successfully injected
func (m *Machine) verifyBootstrapInjection(ctx gocontext.Context, vm *kubevirtv1.VirtualMachine, expectedData string) error {
	logger := m.machineContext.Logger.WithValues("component", "bootstrap-verification")
	
	// Refresh VM object to get latest state
	namespacedName := types.NamespacedName{Namespace: vm.Namespace, Name: vm.Name}
	if err := m.client.Get(ctx, namespacedName, vm); err != nil {
		return errors.Wrapf(err, "failed to refresh VM for verification")
	}
	
	// Verify VM is running
	currentState := string(vm.Status.PrintableStatus)
	if currentState != "Running" {
		return fmt.Errorf("VM not in running state after restart: %s", currentState)
	}
	
	// Verify cloud-init data was updated
	for _, volume := range vm.Spec.Template.Spec.Volumes {
		if volume.Name == "cloudinit" && volume.CloudInitNoCloud != nil {
			actualData := volume.CloudInitNoCloud.UserData
			if actualData != expectedData {
				logger.Info("Bootstrap data mismatch detected", 
					"expectedSize", len(expectedData), 
					"actualSize", len(actualData),
					"matches", actualData == expectedData)
				return fmt.Errorf("bootstrap data verification failed: data mismatch")
			}
			logger.Info("Bootstrap data verification successful", "dataSize", len(actualData))
			return nil
		}
	}
	
	return fmt.Errorf("cloud-init volume not found during verification")
}

// min helper function
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
