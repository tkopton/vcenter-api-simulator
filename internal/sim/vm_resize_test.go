package sim

import (
	"context"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// resizeSim is a vCenter with an undersized VM (1) and an oversized VM (2)
// in its first cluster, a SOAP client for it, and a channel that hears of
// every resize a tool makes.
func resizeSim(t *testing.T) (*Simulator, *vim25.Client, chan struct{}) {
	t.Helper()
	sc := lifeScenario(t)
	sc.Defaults.Host.Threads = 32
	sc.Clusters = []scenario.ClusterOverride{{Key: "dc-01/cluster-01", VMRoles: map[int]string{1: scenario.RoleUndersized, 2: scenario.RoleOversized}}}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	reg := registry.InMemory(sc.Seed)
	resized := make(chan struct{}, 16)
	s, err := New(Options{Topology: topology.Generate(sc, reg), Registry: reg, VMResized: func() { resized <- struct{}{} },
		Now: func() time.Time { return time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	srv := httptest.NewServer(s.Handler)
	t.Cleanup(srv.Close)
	u, _ := soap.ParseURL(srv.URL + "/sdk")
	u.User = nil
	ctx := context.Background()
	c, err := vim25.NewClient(ctx, soap.NewClient(u, true))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.NewManager(c).Login(ctx, url.UserPassword("user", "pass")); err != nil {
		t.Fatal(err)
	}
	return s, c, resized
}

// waitTask waits for the task a call started: waitTask(t, what)(call).
func waitTask(t *testing.T, what string) func(*object.Task, error) {
	return func(task *object.Task, err error) {
		t.Helper()
		if err == nil {
			err = task.Wait(context.Background())
		}
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
}

func heard(t *testing.T, resized chan struct{}) {
	t.Helper()
	select {
	case <-resized:
	case <-time.After(5 * time.Second):
		t.Fatal("the controller was not told of the resize")
	}
}

// VCF Operations' "Set CPU Count and Memory for VM" action snapshots the
// VM, powers it off, reconfigures it and powers it on again. The VM must
// then be that size everywhere a tool reads it, the simulator must report
// the size to keep, and the undersized VM's workload must fit its new size.
func TestToolResizesAnUndersizedVM(t *testing.T) {
	s, c, resized := resizeSim(t)
	refs := s.Clusters[0]
	ref := refs.VMs[0]
	if vm := refs.Cluster.VMs[0]; vm.Role != scenario.RoleUndersized || vm.Index != 0 {
		t.Fatalf("first VM is %s with role %q", vm.Name, vm.Role)
	}
	at := s.Env.Now()
	before := readPerformanceState(s.Life().as("")).sample(s.Topology.TelemetrySeed, at)[ref]
	if before.demand <= before.cpuCap || before.balloon == 0 {
		t.Fatalf("undersized VM before the resize: demand %d of %d MHz, balloon %d MB", before.demand, before.cpuCap, before.balloon)
	}

	ctx := context.Background()
	vm := object.NewVirtualMachine(c, ref)
	waitTask(t, "snapshot")(vm.CreateSnapshot(ctx, "VCF Operations rightsizing", "", false, false))
	waitTask(t, "power off")(vm.PowerOff(ctx))
	var off mo.VirtualMachine
	if err := property.DefaultCollector(c).RetrieveOne(ctx, ref, []string{"guest", "runtime"}, &off); err != nil {
		t.Fatal(err)
	}
	if off.Guest.GuestState != string(types.VirtualMachineGuestStateNotRunning) || off.Guest.ToolsRunningStatus != string(types.VirtualMachineToolsRunningStatusGuestToolsNotRunning) || off.Runtime.BootTime != nil {
		t.Errorf("powered off by a tool: guest %s, tools %s, boot %v", off.Guest.GuestState, off.Guest.ToolsRunningStatus, off.Runtime.BootTime)
	}
	waitTask(t, "reconfigure")(vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{NumCPUs: 4, MemoryMB: 8192}))
	heard(t, resized)
	waitTask(t, "power on")(vm.PowerOn(ctx))

	var got mo.VirtualMachine
	if err := property.DefaultCollector(c).RetrieveOne(ctx, ref, []string{"config.hardware", "summary.config", "runtime", "guest"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Guest.GuestState != "running" || got.Guest.ToolsRunningStatus != string(types.VirtualMachineToolsRunningStatusGuestToolsRunning) || got.Guest.IpAddress == "" {
		t.Errorf("powered on by a tool: guest %s, tools %s, address %q", got.Guest.GuestState, got.Guest.ToolsRunningStatus, got.Guest.IpAddress)
	}
	if got.Runtime.BootTime == nil || !got.Runtime.BootTime.Equal(s.Env.Now()) {
		t.Errorf("powered on by a tool at %v: boot time %v", s.Env.Now(), got.Runtime.BootTime)
	}
	hw, sum, rt := got.Config.Hardware, got.Summary.Config, got.Runtime
	if hw.NumCPU != 4 || hw.MemoryMB != 8192 || sum.NumCpu != 4 || sum.MemorySizeMB != 8192 {
		t.Errorf("configured %d vCPUs %d MB, summary %d vCPUs %d MB; want 4 and 8192", hw.NumCPU, hw.MemoryMB, sum.NumCpu, sum.MemorySizeMB)
	}
	if rt.MaxCpuUsage != 4*cpuMhz || rt.MaxMemoryUsage != 8192 || rt.PowerState != types.VirtualMachinePowerStatePoweredOn {
		t.Errorf("runtime: max %d MHz %d MB, %s", rt.MaxCpuUsage, rt.MaxMemoryUsage, rt.PowerState)
	}
	sizes := s.VMSizes()
	if len(sizes) != 1 || len(sizes["dc-01/cluster-01"]) != 1 || sizes["dc-01/cluster-01"][1] != (scenario.VMSize{CPUs: 4, MemoryMB: 8192}) {
		t.Errorf("sizes to keep: %v", sizes)
	}
	if roleOf(s.Life().as(""), ref) != scenario.RoleUndersized {
		t.Error("the VM lost its role")
	}

	// over a whole day the VM now has room for what its guest asks for
	for h := 0; h < 24; h++ {
		w := readPerformanceState(s.Life().as("")).sample(s.Topology.TelemetrySeed, at.Add(time.Duration(h)*time.Hour))[ref]
		if w.cpuCap != 4*int64(cpuMhz) || w.memCap != 8192 {
			t.Fatalf("%02d:00 capacity %d MHz %d MB", h, w.cpuCap, w.memCap)
		}
		if w.demand*10 > w.cpuCap*9 || w.balloon != 0 || w.swapped != 0 || w.memory*10 > w.memCap*6 {
			t.Errorf("%02d:00 resized undersized VM: demand %d of %d MHz, memory %d of %d MB, balloon %d, swapped %d", h, w.demand, w.cpuCap, w.memory, w.memCap, w.balloon, w.swapped)
		}
		if w.demand < before.cpuCap*9/10 {
			t.Errorf("%02d:00 the guest's work shrank with the resize: demand %d MHz", h, w.demand)
		}
	}
}

// An oversized VM made smaller keeps its workload: the same MHz is a
// larger share of what it has. A reconfigure that leaves the size alone is
// not a resize.
func TestToolShrinksAnOversizedVM(t *testing.T) {
	s, c, resized := resizeSim(t)
	refs := s.Clusters[0]
	ref := refs.VMs[1]
	if vm := refs.Cluster.VMs[1]; vm.Role != scenario.RoleOversized {
		t.Fatalf("second VM is %s with role %q", vm.Name, vm.Role)
	}
	at := s.Env.Now()
	before := readPerformanceState(s.Life().as("")).sample(s.Topology.TelemetrySeed, at)[ref]
	ctx := context.Background()
	vm := object.NewVirtualMachine(c, ref)
	waitTask(t, "annotate")(vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{Annotation: "owner: finance"}))
	waitTask(t, "shrink")(vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{NumCPUs: 4, MemoryMB: 16384}))
	heard(t, resized)
	select {
	case <-resized:
		t.Error("the annotation was taken for a resize")
	default:
	}
	after := readPerformanceState(s.Life().as("")).sample(s.Topology.TelemetrySeed, at)[ref]
	if after.cpuCap*2 != before.cpuCap || after.memCap*2 != before.memCap {
		t.Fatalf("capacity %d MHz %d MB, was %d %d", after.cpuCap, after.memCap, before.cpuCap, before.memCap)
	}
	if d := after.cpu - before.cpu; d < -2 || d > 2 || after.memory != before.memory {
		t.Errorf("the workload changed with the size: %d MHz %d MB, was %d %d", after.cpu, after.memory, before.cpu, before.memory)
	}
	if s.VMSizes()["dc-01/cluster-01"][2] != (scenario.VMSize{CPUs: 4, MemoryMB: 16384}) {
		t.Errorf("sizes to keep: %v", s.VMSizes())
	}
	// back to the size it was built with: nothing left to keep
	waitTask(t, "grow back")(vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{NumCPUs: topology.OversizedCPUs, MemoryMB: topology.OversizedMemoryMB}))
	heard(t, resized)
	if sizes := s.VMSizes(); len(sizes) != 0 {
		t.Errorf("sizes to keep after the VM is back to its built size: %v", sizes)
	}
}

// A VM built at a kept size (scenario vmSizes) has that size, and the
// workload of the size its role gives it.
func TestKeptSizeIsBuilt(t *testing.T) {
	sc := lifeScenario(t)
	sc.Defaults.Host.Threads = 32
	sc.Clusters = []scenario.ClusterOverride{{Key: "dc-01/cluster-01", VMRoles: map[int]string{1: scenario.RoleUndersized},
		VMSizes: map[int]scenario.VMSize{1: {CPUs: 4, MemoryMB: 8192}}}}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	s := lifeSim(t, sc, registry.InMemory(sc.Seed))
	refs := s.Clusters[0]
	vm := refs.Cluster.VMs[0]
	if vm.CPUs != 4 || vm.MemoryMB != 8192 || vm.BuiltCPUs != topology.VMCPUs || vm.BuiltMemoryMB != topology.VMMemoryMB {
		t.Fatalf("%s: %d vCPUs %d MB, built %d %d", vm.Name, vm.CPUs, vm.MemoryMB, vm.BuiltCPUs, vm.BuiltMemoryMB)
	}
	w := readPerformanceState(s.Life().as("")).sample(s.Topology.TelemetrySeed, s.Env.Now())[refs.VMs[0]]
	if w.cpuCap != 4*int64(cpuMhz) || w.demand*10 > w.cpuCap*9 || w.balloon != 0 {
		t.Errorf("kept size: demand %d of %d MHz, balloon %d", w.demand, w.cpuCap, w.balloon)
	}
	if len(s.VMSizes()["dc-01/cluster-01"]) != 1 {
		t.Errorf("the kept size is not reported to keep: %v", s.VMSizes())
	}
}
