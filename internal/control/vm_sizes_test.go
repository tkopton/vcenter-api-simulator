package control

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

// A VM a tool resized (a rightsizing action) stays that size: the
// controller keeps the size in the scenario, a restart builds the VM at
// it, and a changed scenario file that says nothing of it keeps it too.
func TestResizedVMStaysResized(t *testing.T) {
	sized := func() *scenario.Scenario {
		sc := lively(t)
		sc.Auth.AnyLogin = true
		sc.Clusters = []scenario.ClusterOverride{{Key: "dc-01/cluster-01", VMRoles: map[int]string{5: scenario.RoleUndersized}}}
		if err := sc.Validate(); err != nil {
			t.Fatal(err)
		}
		return sc
	}
	dir := t.TempDir()
	c := newCtl(t, sized(), dir, "first")
	r := c.applied()
	refs := r.sim.Clusters[0]
	var ref types.ManagedObjectReference
	for i, vm := range refs.Cluster.VMs {
		if vm.Index == 4 {
			ref = refs.VMs[i]
		}
	}
	srv := httptest.NewServer(r.sim.Handler)
	defer srv.Close()
	u, _ := soap.ParseURL(srv.URL + "/sdk")
	u.User = nil
	ctx := context.Background()
	client, err := vim25.NewClient(ctx, soap.NewClient(u, true))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.NewManager(client).Login(ctx, url.UserPassword("vcf-ops", "any")); err != nil {
		t.Fatal(err)
	}
	vm := object.NewVirtualMachine(client, ref)
	task, err := vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{NumCPUs: 4, MemoryMB: 8192})
	if err == nil {
		err = task.Wait(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	kept := func(c *Controller) scenario.VMSize {
		return c.applied().sc.VMSizes("dc-01/cluster-01")[5]
	}
	want := scenario.VMSize{CPUs: 4, MemoryMB: 8192}
	for end := time.Now().Add(5 * time.Second); kept(c) != want && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	if got := kept(c); got != want {
		t.Fatalf("the controller kept %+v", got)
	}
	if h := history(c); !strings.Contains(h, "A tool resized "+vmName(r.topo.Clusters()[0], 5)+" to 4 vCPUs and 8 GB.") {
		t.Errorf("history does not say what the tool did:\n%s", h)
	}
	c.Close()

	built := func(c *Controller) (int, int, int) {
		for _, vm := range c.applied().topo.Clusters()[0].VMs {
			if vm.Index == 4 {
				return vm.CPUs, vm.MemoryMB, vm.BuiltCPUs
			}
		}
		return 0, 0, 0
	}
	again := newCtl(t, sized(), dir, "first")
	if cpus, mem, base := built(again); cpus != 4 || mem != 8192 || base != 2 {
		t.Errorf("after a restart the VM is %d vCPUs %d MB, built for %d vCPUs", cpus, mem, base)
	}
	again.Close()

	// the scenario file changed, and says nothing of the VM's size
	changed := newCtl(t, sized(), dir, "second")
	if cpus, mem, _ := built(changed); cpus != 4 || mem != 8192 {
		t.Errorf("after the scenario file changed the VM is %d vCPUs %d MB", cpus, mem)
	}
}
