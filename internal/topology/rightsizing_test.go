package topology

import (
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

func rightsizingScenario(t *testing.T, rs *scenario.Rightsizing, pins map[int]string) *scenario.Scenario {
	t.Helper()
	sc := scenario.Phase1("rightsizing")
	sc.Defaults.VMs.PerHost = scenario.IntPtr(25)
	sc.Defaults.VMs.Rightsizing = rs
	if pins != nil {
		sc.Clusters = []scenario.ClusterOverride{{Key: "dc-01/cluster-01", VMRoles: pins}}
	}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	return sc
}

func roles(tp *Topology) map[string]string {
	out := map[string]string{}
	for _, vm := range tp.Datacenters[0].Clusters[0].VMs {
		out[vm.Name] = vm.Role
	}
	return out
}

// Roles are dealt by each VM's identity: the same scenario deals the same
// roles, every role gets a share near its percentage, a VM's size and
// snapshot follow its role, and a pinned VM keeps its pin.
func TestRightsizingRoles(t *testing.T) {
	rs := &scenario.Rightsizing{Idle: 20, PoweredOff: 20, Oversized: 20, Undersized: 20, OldSnapshots: 20, SnapshotAgeDays: &scenario.Range{Min: 30, Max: 40}}
	sc := rightsizingScenario(t, rs, nil)
	a := Generate(sc, registry.InMemory(sc.Seed))
	b := Generate(sc, registry.InMemory(sc.Seed))
	if ra, rb := roles(a), roles(b); len(ra) != 100 || !equal(ra, rb) {
		t.Fatalf("roles differ between two builds of one scenario, or wrong VM count %d", len(ra))
	}
	count := map[string]int{}
	for _, vm := range a.Datacenters[0].Clusters[0].VMs {
		count[vm.Role]++
		switch vm.Role {
		case scenario.RoleOversized:
			if vm.CPUs != OversizedCPUs || vm.MemoryMB != OversizedMemoryMB {
				t.Errorf("%s oversized but %d vCPU %d MB", vm.Name, vm.CPUs, vm.MemoryMB)
			}
		case scenario.RoleOldSnapshot:
			if vm.SnapshotAge < 30*24*time.Hour || vm.SnapshotAge > 40*24*time.Hour || vm.OldSnapshotBytes < snapshotMinGB<<30 || vm.OldSnapshotBytes > snapshotMaxGB<<30 || vm.SnapshotName == "" {
				t.Errorf("%s snapshot age %v size %d name %q", vm.Name, vm.SnapshotAge, vm.OldSnapshotBytes, vm.SnapshotName)
			}
		default:
			if vm.CPUs != VMCPUs || vm.MemoryMB != VMMemoryMB || vm.SnapshotAge != 0 {
				t.Errorf("%s (%q) has %d vCPU %d MB snapshot %v", vm.Name, vm.Role, vm.CPUs, vm.MemoryMB, vm.SnapshotAge)
			}
		}
	}
	for _, role := range scenario.Roles {
		if count[role] < 8 || count[role] > 32 {
			t.Errorf("%s: %d of 100 VMs for a 20%% share", role, count[role])
		}
	}
	if count[""] != 0 {
		t.Errorf("%d VMs without a role when the shares add up to 100", count[""])
	}

	// pins win over the shares, "normal" included
	pinned := Generate(rightsizingScenario(t, rs, map[int]string{1: scenario.RoleUndersized, 2: scenario.RoleNormal}), registry.InMemory(sc.Seed))
	vms := pinned.Datacenters[0].Clusters[0].VMs
	if vms[0].Role != scenario.RoleUndersized || vms[1].Role != "" {
		t.Fatalf("pinned roles %q %q", vms[0].Role, vms[1].Role)
	}

	// a VM without a role in a cluster that gives roles carries a load its
	// size fits; the creation date is the same on every build and long past
	for i, vm := range pinned.Datacenters[0].Clusters[0].VMs {
		if vm.RightSized != (vm.Role == "") {
			t.Errorf("%s (%q): right-sized %v", vm.Name, vm.Role, vm.RightSized)
		}
		if again := Generate(rightsizingScenario(t, rs, map[int]string{1: scenario.RoleUndersized, 2: scenario.RoleNormal}), registry.InMemory(sc.Seed)).Datacenters[0].Clusters[0].VMs[i]; !again.Created.Equal(vm.Created) || !vm.Created.Before(createdBefore.Add(time.Second)) || vm.Created.Before(createdBefore.AddDate(-4, 0, 0)) {
			t.Errorf("%s created %v, then %v", vm.Name, vm.Created, again.Created)
		}
		if i > 3 {
			break
		}
	}

	// no shares and no pins: every VM as built
	plain := Generate(rightsizingScenario(t, nil, nil), registry.InMemory(sc.Seed))
	for _, vm := range plain.Datacenters[0].Clusters[0].VMs {
		if vm.Role != "" || vm.RightSized || vm.CPUs != VMCPUs || vm.MemoryMB != VMMemoryMB {
			t.Fatalf("%s without shares: role %q, right-sized %v, %d vCPU %d MB", vm.Name, vm.Role, vm.RightSized, vm.CPUs, vm.MemoryMB)
		}
	}
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
