package sim

import (
	"testing"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// rightsizingSim is lifeScenario's two clusters with a share of every role
// and one pinned VM.
func rightsizingSim(t *testing.T) *Simulator {
	t.Helper()
	sc := lifeScenario(t)
	sc.Defaults.Host.Threads = 32
	sc.Defaults.VMs.Rightsizing = &scenario.Rightsizing{Idle: 15, PoweredOff: 15, Oversized: 15, Undersized: 15, OldSnapshots: 15}
	sc.Clusters = []scenario.ClusterOverride{{Key: "dc-01/cluster-01", VMRoles: map[int]string{1: scenario.RoleUndersized}}}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	return lifeSim(t, sc, registry.InMemory(sc.Seed))
}

// Each role shows what capacity tools look for, in the inventory and in the
// performance values; VMs without a role are as built.
func TestRightsizingRolesInInventoryAndPerformance(t *testing.T) {
	s := rightsizingSim(t)
	l := s.Life()
	ctx := l.as("")
	now := s.Env.Now()
	samples := readPerformanceState(ctx).sample(s.Topology.TelemetrySeed, now)
	seen := map[string]int{}
	for _, refs := range s.Clusters {
		off := 0
		for i, vm := range refs.Cluster.VMs {
			obj := l.reg.Get(refs.VMs[i]).(*simulator.VirtualMachine)
			var cpus, mem int32
			var state types.VirtualMachinePowerState
			var snap *types.VirtualMachineSnapshotInfo
			var files []types.VirtualMachineFileLayoutExFileInfo
			var committed int64
			ctx.WithLock(obj, func() {
				cpus, mem, state, snap = obj.Config.Hardware.NumCPU, obj.Config.Hardware.MemoryMB, obj.Runtime.PowerState, obj.Snapshot
				if obj.LayoutEx != nil {
					files = obj.LayoutEx.File
				}
				if obj.Summary.Storage != nil {
					committed = obj.Summary.Storage.Committed
				}
			})
			if int(cpus) != vm.CPUs || int(mem) != vm.MemoryMB {
				t.Errorf("%s: configured %d vCPU %d MB, topology says %d %d", vm.Name, cpus, mem, vm.CPUs, vm.MemoryMB)
			}
			if obj.Runtime.MaxCpuUsage != int32(vm.CPUs)*cpuMhz {
				t.Errorf("%s: maxCpuUsage %d", vm.Name, obj.Runtime.MaxCpuUsage)
			}
			w := samples[refs.VMs[i]]
			seen[vm.Role]++
			switch vm.Role {
			case scenario.RolePoweredOff:
				off++
				if state != types.VirtualMachinePowerStatePoweredOff || w.cpu != 0 || w.memory != 0 || !l.off[refs.Cluster.Key][vm.Name] {
					t.Errorf("%s powered off: state %s, cpu %d, memory %d, Activity knows it is off: %v", vm.Name, state, w.cpu, w.memory, l.off[refs.Cluster.Key][vm.Name])
				}
			case scenario.RoleIdle:
				if w.cpu >= 100 || w.memory > int64(vm.MemoryMB)/5 || w.read+w.write > 10 {
					t.Errorf("%s idle: %d MHz, %d MB, %d KBps disk", vm.Name, w.cpu, w.memory, w.read+w.write)
				}
			case scenario.RoleOversized:
				if w.cpu*10 > w.cpuCap || w.memory*4 > w.memCap {
					t.Errorf("%s oversized: %d of %d MHz, %d of %d MB", vm.Name, w.cpu, w.cpuCap, w.memory, w.memCap)
				}
			case scenario.RoleUndersized:
				if w.demand != w.cpuCap || w.memory*10 < w.memCap*9 || w.balloon == 0 || w.swapped == 0 || w.ready*100 < 8*20000*w.vcpus {
					t.Errorf("%s undersized: demand %d of %d MHz, %d of %d MB, balloon %d swapped %d, ready %d ms", vm.Name, w.demand, w.cpuCap, w.memory, w.memCap, w.balloon, w.swapped, w.ready)
				}
			case scenario.RoleOldSnapshot:
				if snap == nil || len(snap.RootSnapshotList) != 1 {
					t.Fatalf("%s has no old snapshot", vm.Name)
				}
				taken := snap.RootSnapshotList[0]
				if age := now.Sub(taken.CreateTime); age < vm.SnapshotAge-time.Second || age > vm.SnapshotAge+time.Second || taken.Name != vm.SnapshotName {
					t.Errorf("%s snapshot %q is %v old, want %v", vm.Name, taken.Name, age, vm.SnapshotAge)
				}
				sized := false
				for _, f := range files {
					sized = sized || f.Size == vm.OldSnapshotBytes
				}
				if !sized {
					t.Errorf("%s: no snapshot file of %d bytes in layoutEx", vm.Name, vm.OldSnapshotBytes)
				}
				// the storage model counts the snapshot, so the datastore does too
				if want, _, _ := topology.VMStorage(vm); vm.SnapshotBytes() != vm.OldSnapshotBytes || committed != want {
					t.Errorf("%s: storage model has a %d byte snapshot, VM commits %d of %d", vm.Name, vm.SnapshotBytes(), committed, want)
				}
			case "":
				if state != types.VirtualMachinePowerStatePoweredOn || snap != nil {
					t.Errorf("%s as built: %s, snapshot %v", vm.Name, state, snap != nil)
				}
			}
		}
		sum := l.reg.Get(refs.Ref).(*simulator.ClusterComputeResource).Summary.(*types.ClusterComputeResourceSummary)
		if int(sum.UsageSummary.PoweredOffVmCount) != off {
			t.Errorf("%s: summary counts %d powered-off VMs, %d are", refs.Cluster.Key, sum.UsageSummary.PoweredOffVmCount, off)
		}
	}
	for _, role := range append([]string{""}, scenario.Roles...) {
		if seen[role] == 0 {
			t.Errorf("no VM with role %q among %v", role, seen)
		}
	}
	if vm := s.Clusters[0].Cluster.VMs[0]; vm.Role != scenario.RoleUndersized {
		t.Errorf("pinned VM %s has role %q", vm.Name, vm.Role)
	}
}

// Activity powers off, removes and backs up only VMs without a role, so a
// role lasts while Activity runs.
func TestActivityLeavesRoleVMsAlone(t *testing.T) {
	s := rightsizingSim(t)
	l := s.Life()
	for _, refs := range s.Clusters {
		key := refs.Cluster.Key
		role := map[string]string{}
		for _, vm := range refs.Cluster.VMs {
			role[vm.Name] = vm.Role
		}
		for _, name := range l.VMNames(key) {
			if role[name] != "" {
				t.Errorf("backups would snapshot %s (%s)", name, role[name])
			}
		}
		for pick := uint64(0); pick < 6; pick++ {
			if name := l.PowerOff(key, pick*7919); name != "" && role[name] != "" {
				t.Fatalf("Activity powered off %s (%s)", name, role[name])
			}
		}
		for pick := uint64(0); pick < 3; pick++ {
			name, err := l.RemoveVM(key, pick*104729)
			if err != nil {
				t.Fatal(err)
			}
			if role[name] != "" {
				t.Fatalf("Activity removed %s (%s)", name, role[name])
			}
		}
	}
	// the saved Activity state holds what Activity powered off, not the VMs
	// built powered off
	built := map[string]bool{}
	for _, refs := range s.Clusters {
		for _, vm := range refs.Cluster.VMs {
			if vm.Role == scenario.RolePoweredOff {
				built[vm.Key] = true
			}
		}
	}
	for _, k := range l.Snapshot().Off {
		if built[k] {
			t.Errorf("saved state powers %s off again on restore", k)
		}
	}
}
