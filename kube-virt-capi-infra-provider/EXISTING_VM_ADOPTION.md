# Existing VM Adoption for KubeVirt Provider

This document describes how to adopt existing KubeVirt VMs into CAPI management using the enhanced KubeVirt provider.

## Overview

The KubeVirt provider now supports adopting existing VMs without requiring VM restarts. This feature is particularly useful for:

- Migrating existing infrastructure to CAPI management
- Integrating with Kamaji hosted control planes
- Adding existing worker nodes to CAPI clusters

## How It Works

The adoption process uses an annotation-based approach:

1. **Detection**: Controller detects `capk.cluster.x-k8s.io/existing-vm-name` annotation
2. **Validation**: Verify VM exists and is compatible (Running state, guest agent optional)
3. **Label Application**: Apply CAPI management labels to the VM
4. **Cloud-Init Injection**: Use existing bootstrap secret mechanism for CAPI integration
5. **Lifecycle Integration**: Standard CAPI machine lifecycle management

## Prerequisites

- Existing KubeVirt VM in `Running` state
- VM should have guest agent for optimal integration (optional but recommended)
- Access to the VM namespace from CAPI infrastructure provider

## Usage Example

### 1. Existing VM

Assume you have an existing VM named `lab-vm-01`:

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: lab-vm-01
  namespace: vm-lab
  labels:
    kubevirt.io/vm: lab-vm-01
spec:
  runStrategy: Always
  template:
    spec:
      domain:
        cpu:
          cores: 1
        resources:
          requests:
            cpu: "1"
            memory: 2Gi
```

### 2. Create KubevirtMachine with Adoption Annotation

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: KubevirtMachine
metadata:
  name: worker-1
  namespace: default
  annotations:
    capk.cluster.x-k8s.io/existing-vm-name: "lab-vm-01"
spec:
  virtualMachineTemplate:
    spec:
      runStrategy: Always
      template:
        spec:
          domain:
            cpu:
              cores: 1
            resources:
              requests:
                cpu: "1"
                memory: 2Gi
  infraClusterSecretRef:
    name: vm-lab-kubeconfig
    namespace: default
```

### 3. Create Machine Resource

```yaml
apiVersion: cluster.x-k8s.io/v1beta1
kind: Machine
metadata:
  name: worker-1
  namespace: default
  labels:
    cluster.x-k8s.io/cluster-name: my-cluster
spec:
  clusterName: my-cluster
  bootstrap:
    configRef:
      apiVersion: bootstrap.cluster.x-k8s.io/v1beta1
      kind: KubeadmConfig
      name: worker-1
  infrastructureRef:
    apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
    kind: KubevirtMachine
    name: worker-1
```

## What Happens During Adoption

1. **Controller Detection**: KubeVirt machine controller detects the annotation
2. **VM Lookup**: Finds the existing VM in the specified namespace
3. **Validation**: Checks VM is in Running state and validates compatibility
4. **Label Application**: Applies CAPI management labels:
   ```yaml
   labels:
     cluster.x-k8s.io/cluster-name: "my-cluster"
     cluster.x-k8s.io/role: "worker"
     capk.cluster.x-k8s.io/kubevirt-machine-name: "worker-1"
     capk.cluster.x-k8s.io/kubevirt-machine-namespace: "default"
   ```
5. **Bootstrap Integration**: Creates and injects CAPI bootstrap secret using existing cloud-init mechanism
6. **Provider ID**: Generates CAPI-compatible provider ID: `kubevirt://worker-1`
7. **Node Integration**: Updates workload cluster node with provider ID

## Adoption Process Flow

```
┌─────────────────────┐
│ Existing VM         │
│ (lab-vm-01)         │
│ Status: Running     │
└─────────┬───────────┘
          │
          │ 1. Annotation Detection
          ▼
┌─────────────────────┐
│ KubevirtMachine     │
│ annotations:        │
│  existing-vm-name   │
└─────────┬───────────┘
          │
          │ 2. VM Adoption
          ▼
┌─────────────────────┐
│ Adopted VM          │
│ + CAPI Labels       │
│ + Bootstrap Secret  │
│ + Provider ID       │
└─────────┬───────────┘
          │
          │ 3. Node Integration
          ▼
┌─────────────────────┐
│ CAPI Managed VM     │
│ Full Lifecycle      │
│ Bootstrap Complete  │
└─────────────────────┘
```

## Benefits

- ✅ **Zero Downtime**: No VM restart required
- ✅ **Proven Mechanism**: Reuses existing cloud-init injection
- ✅ **Backward Compatible**: No breaking changes
- ✅ **Full Integration**: Complete CAPI lifecycle management
- ✅ **Error Handling**: Robust validation and error recovery

## Troubleshooting

### VM Not Found

```
Error: failed to find existing VM: lab-vm-01
```

**Solution**: Ensure the VM exists in the correct namespace and the infraClusterSecretRef points to the right cluster.

### VM Not Running

```
Error: VM must be in Running state, current: Stopped
```

**Solution**: Start the VM before attempting adoption:
```bash
kubectl patch vm lab-vm-01 -n vm-lab --type merge -p '{"spec":{"runStrategy":"Always"}}'
```

### Guest Agent Warning

```
Warning: VM guest agent not connected - some features may be limited
```

**Impact**: Adoption will proceed but some advanced features may be limited. Install qemu-guest-agent in the VM for full functionality.

## Integration with Kamaji

For Kamaji hosted control planes, the adoption process works seamlessly:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: KubevirtCluster
metadata:
  annotations:
    cluster.x-k8s.io/managed-by: kamaji
spec:
  infraClusterSecretRef:
    name: workload-cluster-kubeconfig
```

The adopted VMs will automatically integrate with the Kamaji-managed control plane using the existing cloud-init and bootstrap mechanisms.

## Monitoring Adoption

Check adoption status using kubectl:

```bash
# Check KubevirtMachine status
kubectl get kubevirtmachine worker-1 -o yaml

# Check VM labels after adoption
kubectl get vm lab-vm-01 -n vm-lab -o yaml

# Check conditions
kubectl get kubevirtmachine worker-1 -o jsonpath='{.status.conditions}'
```

## Limitations

- VM must be in `Running` state for adoption
- Existing VM configuration should be compatible with CAPI requirements
- Network and storage configurations are inherited from existing VM
- Bootstrap data injection requires VM restart or guest agent for immediate effect

## Migration from Manual Management

For existing manually-managed KubeVirt VMs:

1. Ensure VMs have required resources (CPU, memory, network)
2. Install guest agent for better integration
3. Create CAPI resources with adoption annotations
4. Monitor adoption process through conditions
5. Validate node integration in workload cluster

This enables smooth migration from manual VM management to full CAPI automation.