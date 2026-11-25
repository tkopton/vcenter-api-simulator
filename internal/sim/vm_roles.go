package sim

import (
	"fmt"
	"sync"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/types"
)

// VM roles (scenario vms.rightsizing and vmRoles) give capacity tools such
// as VCF Operations something to find: idle, powered-off, oversized and
// undersized VMs and old snapshots. A role lasts: the VM is built with it,
// keeps it across restarts, and Activity leaves it alone.

// roleRef is where the performance model finds each VM's role, as loadRef
// is where it finds each cluster's load.
var roleRef = types.ManagedObjectReference{Type: "VcsimulatorVMRoles", Value: "roles"}

type vmRoles struct {
	types.ManagedObjectReference
	mu sync.RWMutex
	m  map[types.ManagedObjectReference]string
}

func (r *vmRoles) Reference() types.ManagedObjectReference { return r.ManagedObjectReference }

func setRole(ctx *simulator.Context, vm types.ManagedObjectReference, role string) {
	if role == "" {
		return
	}
	r, ok := ctx.Map.Get(roleRef).(*vmRoles)
	if !ok {
		r = &vmRoles{ManagedObjectReference: roleRef, m: map[types.ManagedObjectReference]string{}}
		ctx.Map.Put(r)
	}
	r.mu.Lock()
	r.m[vm] = role
	r.mu.Unlock()
}

// roleOf is a VM's role; "" is a VM as built.
func roleOf(ctx *simulator.Context, vm types.ManagedObjectReference) string {
	r, ok := ctx.Map.Get(roleRef).(*vmRoles)
	if !ok {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.m[vm]
}

// shapeOff gives a VM the state, guest and quick stats of a VM that is
// powered off. The caller holds the VM's lock.
func shapeOff(obj *simulator.VirtualMachine) {
	obj.Runtime.PowerState = types.VirtualMachinePowerStatePoweredOff
	obj.Runtime.BootTime = nil
	obj.Summary.Runtime = obj.Runtime
	obj.Guest.GuestState = string(types.VirtualMachineGuestStateNotRunning)
	obj.Guest.ToolsStatus = types.VirtualMachineToolsStatusToolsNotRunning
	obj.Guest.ToolsRunningStatus = string(types.VirtualMachineToolsRunningStatusGuestToolsNotRunning)
	obj.Guest.IpAddress = ""
	obj.Guest.GuestOperationsReady = types.NewBool(false)
	obj.Summary.Guest.ToolsStatus = obj.Guest.ToolsStatus
	obj.Summary.Guest.ToolsRunningStatus = obj.Guest.ToolsRunningStatus
	obj.Summary.Guest.IpAddress = ""
	obj.Summary.QuickStats = types.VirtualMachineQuickStats{GuestHeartbeatStatus: types.ManagedEntityStatusGray}
	obj.GuestHeartbeatStatus = types.ManagedEntityStatusGray
}

// takeOldSnapshot gives a VM with the oldSnapshot role its snapshot: a real
// one, made by vcsim's snapshot implementation so that the snapshot object,
// tree and file layout are all there, then dated back by the VM's snapshot
// age and given the size of a snapshot that has grown for that long.
func (s *Simulator) takeOldSnapshot(ctx *simulator.Context, obj *simulator.VirtualMachine, vm *topology.VM) error {
	if vm.Role != scenario.RoleOldSnapshot {
		return nil
	}
	body := obj.CreateSnapshotTask(ctx, &types.CreateSnapshot_Task{This: obj.Self, Name: vm.SnapshotName, Description: "Left behind after a change; nobody removed it."})
	if f := body.Fault(); f != nil {
		return fmt.Errorf("sim: snapshot %s: %s", vm.Name, f.String)
	}
	ref := body.(*methods.CreateSnapshot_TaskBody).Res.Returnval
	if s.tasks != nil {
		if _, err := s.tasks.result(ctx, body, ref); err != nil {
			return fmt.Errorf("sim: snapshot %s: %w", vm.Name, err)
		}
	}
	taken := s.Env.Now().Add(-vm.SnapshotAge).UTC().Truncate(time.Second)
	ctx.WithLock(obj, func() {
		if obj.Snapshot == nil {
			return
		}
		var date func([]types.VirtualMachineSnapshotTree)
		date = func(nodes []types.VirtualMachineSnapshotTree) {
			for i := range nodes {
				if nodes[i].Name == vm.SnapshotName {
					nodes[i].CreateTime = taken
				}
				date(nodes[i].ChildSnapshotList)
			}
		}
		date(obj.Snapshot.RootSnapshotList)
		changes := []types.PropertyChange{{Name: "snapshot", Val: obj.Snapshot}, {Name: "rootSnapshot", Val: obj.RootSnapshot}}
		if obj.LayoutEx != nil && obj.Snapshot.CurrentSnapshot != nil {
			keys := map[int32]bool{}
			for _, l := range obj.LayoutEx.Snapshot {
				if l.Key == *obj.Snapshot.CurrentSnapshot {
					keys[l.DataKey] = true
				}
			}
			for i := range obj.LayoutEx.File {
				if keys[obj.LayoutEx.File[i].Key] {
					obj.LayoutEx.File[i].Size = vm.SnapshotBytes
					obj.LayoutEx.File[i].UniqueSize = vm.SnapshotBytes
				}
			}
			obj.Summary.Storage.Committed += vm.SnapshotBytes
			obj.Summary.Storage.Unshared += vm.SnapshotBytes
			changes = append(changes, types.PropertyChange{Name: "layoutEx", Val: obj.LayoutEx}, types.PropertyChange{Name: "summary.storage", Val: obj.Summary.Storage})
		}
		ctx.Update(obj, changes)
	})
	return nil
}

// roleWorkload replaces a running VM's usage with its role's, after the
// usual model has filled the rest. A role does not follow Activity's load
// dials, so that it stays what it is.
func roleWorkload(role string, n *workloadNode, w *workload, day, drift, cpuScale, memScale float64) {
	var usage float64
	switch role {
	case scenario.RoleIdle:
		// well under 100 MHz, almost no I/O: what idle-VM detection looks for
		usage = (.004 + .004*drift) * cpuScale
		w.cpu = int64(float64(n.cpu) * usage)
		w.demand = w.cpu + w.cpu/50
		w.memory = int64(float64(n.memory) * .12 * memScale)
		w.activeMemory = w.memory / 4
		w.read, w.write, w.rx, w.tx = 2, 1, 1, 1
		w.usedCPU = int64(usage * 20000 * float64(n.cpus))
		w.ready = int64(.001 * 20000 * float64(n.cpus))
	case scenario.RoleOversized:
		// far more vCPUs and memory than the workload ever uses
		usage = (.03 + .04*day*drift) * cpuScale
		w.cpu = int64(float64(n.cpu) * usage)
		w.demand = w.cpu + w.cpu/50
		w.memory = int64(float64(n.memory) * (.12 + .06*day) * memScale)
		w.activeMemory = w.memory * 4 / 10
		w.usedCPU = int64(usage * 20000 * float64(n.cpus))
		w.ready = int64(.003 * 20000 * float64(n.cpus))
	case scenario.RoleUndersized:
		// CPU flat out with the guest waiting for more, memory full and
		// under pressure
		usage = min(.98, .9+.08*day*drift) * cpuScale
		readyPct := .08 + .07*day
		w.cpu = int64(float64(n.cpu) * usage)
		w.demand = n.cpu
		w.memory = int64(float64(n.memory) * .97 * memScale)
		w.activeMemory = w.memory * 93 / 100
		w.balloon = n.memory * 4 / 100
		w.swapped = n.memory * 2 / 100
		w.usedCPU = int64(usage * 20000 * float64(n.cpus))
		w.ready = int64(readyPct * 20000 * float64(n.cpus))
	default:
		return
	}
	w.wait = max(0, 20000*int64(n.cpus)-w.usedCPU-w.ready)
	w.ios = max(1, (w.read+w.write)/32)
	w.latency = int64(900+4000*usage) * w.ios
}
