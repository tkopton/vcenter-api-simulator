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
			if obj.Config.CreateDate == nil || !obj.Config.CreateDate.Equal(vm.Created) || !vm.Created.Before(now.AddDate(0, 0, -30)) {
				t.Errorf("%s: created %v, topology says %v", vm.Name, obj.Config.CreateDate, vm.Created)
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
				if w.demand < w.cpuCap*9/10 || w.memory*10 < w.memCap*9 || w.balloon == 0 || w.swapped == 0 || w.ready*100 < 8*20000*w.vcpus {
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

// Over a whole day, an undersized VM wants more than it has, by an amount
// that changes, so a projection lands above its size; a VM without a role in
// a cluster that gives roles carries a load its size fits.
func TestRightsizingLoadsOverADay(t *testing.T) {
	s := rightsizingSim(t)
	ctx := s.Life().as("")
	st := readPerformanceState(ctx)
	start := s.Env.Now()
	type span struct{ lo, hi, sum, n int64 }
	add := func(sp *span, v int64) {
		if sp.n == 0 || v < sp.lo {
			sp.lo = v
		}
		sp.hi = max(sp.hi, v)
		sp.sum += v
		sp.n++
	}
	under, fit := map[string]*[3]span{}, map[string]*[2]span{}
	caps := map[string][2]int64{}
	for i := 0; i < 24*12; i++ { // every 5 minutes for a day
		w := st.sample(s.Topology.TelemetrySeed, start.Add(time.Duration(i)*5*time.Minute))
		for _, refs := range s.Clusters {
			for j, vm := range refs.Cluster.VMs {
				v := w[refs.VMs[j]]
				caps[vm.Name] = [2]int64{v.cpuCap, v.memCap}
				switch {
				case vm.Role == scenario.RoleUndersized:
					if under[vm.Name] == nil {
						under[vm.Name] = &[3]span{}
					}
					add(&under[vm.Name][0], v.demand)
					add(&under[vm.Name][1], v.memory)
					add(&under[vm.Name][2], v.read)
				case vm.RightSized:
					if fit[vm.Name] == nil {
						fit[vm.Name] = &[2]span{}
					}
					add(&fit[vm.Name][0], v.cpu)
					add(&fit[vm.Name][1], v.memory)
				}
			}
		}
	}
	if len(under) == 0 || len(fit) == 0 {
		t.Fatalf("%d undersized and %d right-sized VMs", len(under), len(fit))
	}
	for name, sp := range under {
		c := caps[name]
		if sp[0].hi*100 < c[0]*115 || sp[0].hi-sp[0].lo < c[0]/10 || sp[1].hi < c[1] || sp[2].lo < 20000 {
			t.Errorf("%s undersized: demand %d..%d of %d MHz, memory up to %d of %d MB, paging from %d KBps", name, sp[0].lo, sp[0].hi, c[0], sp[1].hi, c[1], sp[2].lo)
		}
	}
	for name, sp := range fit {
		c := caps[name]
		avg, peak := sp[0].sum*100/sp[0].n/c[0], sp[0].hi*100/c[0]
		mem := sp[1].hi * 100 / c[1]
		// a host overcommitted on vCPUs scales every VM down, so allow less
		if avg < 25 || avg > 65 || peak < 50 || peak > 85 || mem < 55 || mem > 90 {
			t.Errorf("%s right-sized: CPU average %d%%, peak %d%%, memory up to %d%%", name, avg, peak, mem)
		}
	}
}

// A VM Activity adds is made now; the builder's VMs were made long ago.
func TestActivityAddedVMIsNew(t *testing.T) {
	s := rightsizingSim(t)
	l := s.Life()
	key := s.Clusters[0].Cluster.Key
	name, _, err := l.AddVM(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx := l.as("")
	refs := s.Clusters[0]
	for _, ref := range refs.VMs {
		obj := l.reg.Get(ref).(*simulator.VirtualMachine)
		var n string
		var created *time.Time
		ctx.WithLock(obj, func() { n, created = obj.Name, obj.Config.CreateDate })
		if n == name {
			if created == nil || l.now().Sub(*created) > time.Minute {
				t.Fatalf("%s added by Activity reports creation at %v, now is %v", name, created, l.now())
			}
			return
		}
	}
	t.Fatalf("added VM %s not found", name)
}
