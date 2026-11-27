package sim

import (
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// A tool resizes a VM with ReconfigVM_Task, as VCF Operations' "Set CPU
// Count and Memory for VM" action does (after a snapshot and a power off
// when the action asks for them). vcsim sets the new size on the VM but
// announces only config.changeVersion and config.modified; vmRoles, a
// registry handler, sees that change, makes the rest of the VM agree with
// the new size and tells the controller, which keeps the size in the
// scenario (vmSizes) so that the VM stays resized.
//
// The workload stays what the VM's role made it for (vmRecord.built*):
// a VM given what its workload needs stops looking undersized, and an
// oversized VM made smaller uses a larger share of what it has.

func (r *vmRoles) PutObject(*simulator.Context, mo.Reference) {}
func (r *vmRoles) RemoveObject(_ *simulator.Context, ref types.ManagedObjectReference) {
	if ref.Type != "VirtualMachine" {
		return
	}
	r.mu.Lock()
	delete(r.m, ref)
	r.mu.Unlock()
}

// UpdateObject runs under the lock of the VM that changed, held by the task
// that changed it, so it changes the VM directly.
func (r *vmRoles) UpdateObject(ctx *simulator.Context, o mo.Reference, changes []types.PropertyChange) {
	if o.Reference().Type != "VirtualMachine" {
		return
	}
	for _, c := range changes {
		switch c.Name {
		case "config.changeVersion":
			r.reconfigured(ctx, o)
		case "runtime.powerState":
			// vcsim's power tasks and guest shutdown; Activity sets the
			// whole runtime and shapes the VM itself
			if state, ok := c.Val.(types.VirtualMachinePowerState); ok {
				r.powered(ctx, o, state)
			}
		}
	}
}

// reconfigured makes the rest of a VM agree with the size a tool gave it.
func (r *vmRoles) reconfigured(ctx *simulator.Context, o mo.Reference) {
	vm, ok := o.(*mo.VirtualMachine)
	if !ok || vm.Config == nil {
		return
	}
	cpus, memoryMB := int(vm.Config.Hardware.NumCPU), int(vm.Config.Hardware.MemoryMB)
	r.mu.Lock()
	rec := r.m[vm.Self]
	if rec == nil || (rec.cpus == cpus && rec.memoryMB == memoryMB) {
		r.mu.Unlock()
		return
	}
	rec.cpus, rec.memoryMB = cpus, memoryMB
	r.mu.Unlock()
	vm.Runtime.MaxCpuUsage = int32(cpus) * cpuMhz
	vm.Runtime.MaxMemoryUsage = int32(memoryMB)
	vm.Summary.Runtime = vm.Runtime
	vm.Summary.Config.NumCpu = int32(cpus)
	vm.Summary.Config.MemorySizeMB = int32(memoryMB)
	ctx.Update(vm, []types.PropertyChange{
		{Name: "config.hardware.numCPU", Val: vm.Config.Hardware.NumCPU},
		{Name: "config.hardware.memoryMB", Val: vm.Config.Hardware.MemoryMB},
		{Name: "summary.config", Val: vm.Summary.Config},
		{Name: "runtime.maxCpuUsage", Val: vm.Runtime.MaxCpuUsage},
		{Name: "runtime.maxMemoryUsage", Val: vm.Runtime.MaxMemoryUsage},
		{Name: "summary.runtime", Val: vm.Summary.Runtime},
	})
	if r.resized != nil {
		// the controller takes its own lock, and may hold it while it waits
		// for this VM's
		go r.resized()
	}
}

// powered gives a VM a tool powered off or on the guest, runtime and quick
// stats Activity gives one (vcsim leaves the guest running when it powers
// a VM off, and keeps the old boot time when it powers one on). A rightsizing
// action powers the VM off and on around the resize.
func (r *vmRoles) powered(ctx *simulator.Context, o mo.Reference, state types.VirtualMachinePowerState) {
	rec, ok := recordOf(ctx, o.Reference())
	if !ok || rec.vm == nil {
		return
	}
	obj, ok := ctx.Map.Get(o.Reference()).(*simulator.VirtualMachine)
	if !ok {
		return
	}
	switch state {
	case types.VirtualMachinePowerStatePoweredOff:
		shapeOff(obj)
	case types.VirtualMachinePowerStatePoweredOn:
		now := time.Now
		if r.now != nil {
			now = r.now
		}
		shapeVM(obj, rec.vm.Cluster, rec.vm, now)
		boot := now().UTC()
		obj.Runtime.BootTime = &boot
		obj.Summary.Runtime = obj.Runtime
		obj.Summary.QuickStats.UptimeSeconds = 0
	default:
		return
	}
	ctx.Update(obj, append([]types.PropertyChange{
		{Name: "runtime", Val: obj.Runtime}, {Name: "summary", Val: obj.Summary}, {Name: "guest", Val: obj.Guest}, {Name: "guestHeartbeatStatus", Val: obj.GuestHeartbeatStatus},
	}, storageChanges(obj)...))
}

// VMSizes is the size of every VM that is no longer the size its role
// builds it with, by cluster key and VM number: what tools resized.
func (s *Simulator) VMSizes() map[string]map[int]scenario.VMSize {
	r, ok := s.model.Service.Context.Map.Get(roleRef).(*vmRoles)
	if !ok {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]map[int]scenario.VMSize{}
	for _, rec := range r.m {
		if rec.cluster == "" || (rec.cpus == rec.builtCPUs && rec.memoryMB == rec.builtMemoryMB) {
			continue
		}
		if out[rec.cluster] == nil {
			out[rec.cluster] = map[int]scenario.VMSize{}
		}
		out[rec.cluster][rec.number] = scenario.VMSize{CPUs: rec.cpus, MemoryMB: rec.memoryMB}
	}
	return out
}
