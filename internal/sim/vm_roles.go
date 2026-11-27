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

// roleRef is where the performance model finds each VM's role and the size
// its workload is made for, as loadRef is where it finds each cluster's
// load. The same object watches for tools resizing VMs (vm_resize.go).
var roleRef = types.ManagedObjectReference{Type: "VcsimulatorVMRoles", Value: "roles"}

// vmRecord is what the simulator keeps about a VM it made.
type vmRecord struct {
	role    string // "" is a VM as built
	cluster string // topology.Cluster.Key
	number  int    // 1 is <cluster>-vm-001
	// builtCPUs and builtMemoryMB are the size the VM's role builds it
	// with, which its workload is made for; cpus and memoryMB the size it
	// has now.
	builtCPUs, builtMemoryMB int
	cpus, memoryMB           int
	vm                       *topology.VM
}

type vmRoles struct {
	types.ManagedObjectReference
	mu sync.RWMutex
	m  map[types.ManagedObjectReference]*vmRecord
	// resized is told, from its own goroutine, when a tool resized a VM.
	resized func()
	// now is the simulator's clock: a VM a tool powers on boots then.
	now func() time.Time
}

func (r *vmRoles) Reference() types.ManagedObjectReference { return r.ManagedObjectReference }

func vmRecords(ctx *simulator.Context) *vmRoles {
	r, ok := ctx.Map.Get(roleRef).(*vmRoles)
	if !ok {
		r = &vmRoles{ManagedObjectReference: roleRef, m: map[types.ManagedObjectReference]*vmRecord{}}
		ctx.Map.Put(r)
	}
	return r
}

// recordVM keeps what the simulator knows of a VM it made: its role, where
// it belongs and its sizes.
func recordVM(ctx *simulator.Context, ref types.ManagedObjectReference, vm *topology.VM, role string) {
	r := vmRecords(ctx)
	rec := &vmRecord{role: role, cpus: vm.CPUs, memoryMB: vm.MemoryMB, builtCPUs: vm.BuiltCPUs, builtMemoryMB: vm.BuiltMemoryMB}
	if rec.builtCPUs == 0 {
		rec.builtCPUs, rec.builtMemoryMB = vm.CPUs, vm.MemoryMB
	}
	if vm.Cluster != nil {
		rec.cluster = vm.Cluster.Key
	}
	rec.number = vm.Index + 1
	rec.vm = vm
	r.mu.Lock()
	r.m[ref] = rec
	r.mu.Unlock()
}

// recordOf is what the simulator keeps of a VM; false for a VM it did not
// make (one a tool created).
func recordOf(ctx *simulator.Context, vm types.ManagedObjectReference) (vmRecord, bool) {
	r, ok := ctx.Map.Get(roleRef).(*vmRoles)
	if !ok {
		return vmRecord{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.m[vm]
	if !ok {
		return vmRecord{}, false
	}
	return *rec, true
}

// roleOf is a VM's role; "" is a VM as built.
func roleOf(ctx *simulator.Context, vm types.ManagedObjectReference) string {
	rec, _ := recordOf(ctx, vm)
	return rec.role
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
// age. Its size is in the storage model (topology.VMStorage), so the VM's
// storage, its snapshot file and the datastore's used space all count it.
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
		// vcsim recalculated storage from the snapshot's dummy file; put the
		// modelled figures back, the snapshot's size (recorded at build) on
		// its snapshot file, as Activity's backups do
		shapeVMStorage(obj, vm.Cluster, vm, s.Env.Now())
		changes := []types.PropertyChange{{Name: "snapshot", Val: obj.Snapshot}, {Name: "rootSnapshot", Val: obj.RootSnapshot}, {Name: "summary", Val: obj.Summary}}
		changes = append(changes, storageChanges(obj)...)
		ctx.Update(obj, changes)
	})
	return nil
}

// roleRightSized is the workload role of a VM without a role in a cluster
// that gives roles (topology.VM.RightSized): not a scenario role, and
// Activity treats such a VM like any other.
const roleRightSized = "rightSized"

// roleWorkload replaces a running VM's usage with its role's, after the
// usual model has filled the rest. A role does not follow Activity's load
// dials, so that it stays what it is; a right-sized VM does.
//
// A role's load is made for the size the role built the VM with (n.built),
// not the size it has: a tool that resized the VM changed what the VM has,
// not what its guest does. An undersized VM given what it asks for stops
// being short of it, and an oversized VM made smaller uses more of what is
// left, until a capacity tool finds it right.
func roleWorkload(role string, n *workloadNode, w *workload, day, drift, cpuScale, memScale float64) {
	// dayN runs from 0 at night to 1 at the day's peak
	dayN := (day - .40) / .50
	builtCPU, builtMem, builtCPUs := n.built()
	bc, bm := float64(builtCPU), float64(builtMem)
	have := float64(max(1, n.cpu))
	// what the guest's work takes in MHz, the CPU used (no more than the
	// VM has) and CPU time summed over the VM's vCPUs
	use := func(mhz float64) float64 {
		cpu := min(mhz, .95*have) * cpuScale
		w.cpu = int64(cpu)
		w.usedCPU = min(19000*int64(n.cpus), int64(mhz*cpuScale/bc*20000*float64(builtCPUs)))
		return cpu / have
	}
	var usage float64
	switch role {
	case scenario.RoleIdle:
		// well under 100 MHz, almost no I/O: what idle-VM detection looks for
		usage = use(bc * (.004 + .004*drift))
		w.demand = w.cpu + w.cpu/50
		w.memory = min(n.memory, int64(bm*.12*memScale))
		w.activeMemory = w.memory / 4
		w.read, w.write, w.rx, w.tx = 2, 1, 1, 1
		w.ready = int64(.001 * 20000 * float64(n.cpus))
	case scenario.RoleOversized:
		// far more vCPUs and memory than the workload ever uses
		usage = use(bc * (.03 + .04*day*drift))
		w.demand = w.cpu + w.cpu/50
		w.memory = min(n.memory, int64(bm*(.12+.06*day)*memScale))
		w.activeMemory = w.memory * 4 / 10
		w.ready = int64(.003 * 20000 * float64(n.cpus))
	case scenario.RoleUndersized:
		// The guest wants more than it was built with, and how much more
		// changes with the day: a VM pinned at exactly its size would
		// project to exactly its size, and a capacity tool would find
		// nothing to add. CPU demand runs from 92% to 125% of the built
		// vCPUs (above 100%, as turbo boost and contention let it on a real
		// host); the memory the guest needs fills the built VM, and it
		// pages, which counts on top. A VM a tool made large enough has
		// room for all of it.
		want := bc * (.92 + .33*dayN*drift)
		mhz := min(want, .98*have)
		w.cpu = int64(mhz * cpuScale)
		usage = mhz * cpuScale / have
		w.usedCPU = int64(usage * 20000 * float64(n.cpus))
		w.demand = int64(want)
		readyPct := .004 + .01*usage
		if want > .9*have {
			readyPct = .08 + .07*day
		}
		need := bm * min(1, .96+.05*dayN) // the guest's own need: not scaled for the host
		w.memory = min(n.memory, int64(need))
		w.activeMemory = w.memory * 93 / 100
		w.read = int64(200 + 300*dayN)
		if bm*1.06 > float64(n.memory) {
			// short of memory: the host balloons and swaps, and the guest
			// pages (KBps, read back as page-ins)
			w.balloon = builtMem * 4 / 100
			w.swapped = builtMem * 2 / 100
			w.read = int64(20000 + 30000*dayN)
		}
		w.ready = int64(readyPct * 20000 * float64(n.cpus))
	case roleRightSized:
		// A load the VM's size fits: CPU about half used on average and up
		// to 80% at the day's peak, memory 74 to 88% of what it has, so a
		// capacity tool keeps it as it is. Activity's dials still move it.
		usage = use(bc * min(.95, (.35+.45*dayN*drift)*n.loadCPU))
		readyPct := .004 + .01*usage
		w.demand = min(n.cpu, int64(float64(w.cpu)*(1+readyPct)))
		w.memory = min(n.memory, int64(bm*(.74+.14*dayN)*memScale*n.loadMem))
		w.activeMemory = int64(float64(w.memory) * (.45 + .4*usage))
		w.ready = int64(readyPct * 20000 * float64(n.cpus))
	default:
		return
	}
	w.wait = max(0, 20000*int64(n.cpus)-w.usedCPU-w.ready)
	w.ios = max(1, (w.read+w.write)/32)
	w.latency = int64(900+4000*usage) * w.ios
}
