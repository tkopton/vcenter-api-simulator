package sim_test

import (
	"context"
	"testing"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

var (
	hostMemoryCounters = []string{"mem.sysUsage", "mem.heap", "mem.heapfree", "mem.totalCapacity", "mem.unreserved",
		"mem.reservedCapacity", "mem.reservedCapacity.vmOvhd", "mem.reservedCapacity.vmkOvrhd", "mem.reservedCapacity.userworld",
		"mem.reservedCapacityPct", "mem.sharedcommon", "mem.lowfreethreshold", "mem.consumed.userworlds"}
	vmMemoryCounters = []string{"mem.entitlement", "mem.overheadMax", "mem.overheadTouched", "mem.vmmemctltarget"}
)

// Hosts serve the VMkernel and reservation counters a capacity tool needs
// for a host's memory utilization, VMs serve their entitlement, each only
// where vSphere has it, and the numbers agree with each other.
func TestMemoryCountersPerEntity(t *testing.T) {
	e := start(t, "memory")
	ctx := context.Background()
	pm := *e.client.ServiceContent.PerfManager
	var perf mo.PerformanceManager
	if err := property.DefaultCollector(e.client).RetrieveOne(ctx, pm, []string{"perfCounter"}, &perf); err != nil {
		t.Fatal(err)
	}
	keys := map[string]int32{}
	for _, c := range perf.PerfCounter {
		if c.RollupType == types.PerfSummaryTypeAverage || c.RollupType == types.PerfSummaryTypeLatest {
			keys[c.GroupInfo.GetElementDescription().Key+"."+c.NameInfo.GetElementDescription().Key] = c.Key
		}
	}
	advertised := func(ref types.ManagedObjectReference) map[int32]bool {
		r, err := methods.QueryAvailablePerfMetric(ctx, e.client, &types.QueryAvailablePerfMetric{This: pm, Entity: ref, IntervalId: 20})
		if err != nil {
			t.Fatal(err)
		}
		out := map[int32]bool{}
		for _, id := range r.Returnval {
			out[id.CounterId] = true
		}
		return out
	}
	host, vm := e.cl().Hosts[0], e.cl().VMs[0]
	onHost, onVM := advertised(host), advertised(vm)
	for _, name := range hostMemoryCounters {
		if keys[name] == 0 || !onHost[keys[name]] || onVM[keys[name]] {
			t.Errorf("%s: key %d, on host %v, on VM %v; want host only", name, keys[name], onHost[keys[name]], onVM[keys[name]])
		}
	}
	for _, name := range vmMemoryCounters {
		if keys[name] == 0 || !onVM[keys[name]] || onHost[keys[name]] {
			t.Errorf("%s: key %d, on VM %v, on host %v; want VM only", name, keys[name], onVM[keys[name]], onHost[keys[name]])
		}
	}

	sample := func(ref types.ManagedObjectReference, names ...string) map[string]int64 {
		var ids []types.PerfMetricId
		for _, name := range names {
			ids = append(ids, types.PerfMetricId{CounterId: keys[name]})
		}
		r, err := methods.QueryPerf(ctx, e.client, &types.QueryPerf{This: pm, QuerySpec: []types.PerfQuerySpec{{Entity: ref, IntervalId: 20, MaxSample: 1, MetricId: ids}}})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, m := range r.Returnval {
			for _, v := range m.(*types.PerfEntityMetric).Value {
				s := v.(*types.PerfMetricIntSeries)
				for name, key := range keys {
					if key == s.Id.CounterId && len(s.Value) > 0 {
						out[name] = s.Value[len(s.Value)-1]
					}
				}
			}
		}
		return out
	}
	h := sample(host, append(hostMemoryCounters, "mem.overhead")...)
	var hs mo.HostSystem
	if err := property.DefaultCollector(e.client).RetrieveOne(ctx, host, []string{"hardware.memorySize"}, &hs); err != nil {
		t.Fatal(err)
	}
	capKB := hs.Hardware.MemorySize / 1024
	if h["mem.sysUsage"] <= 0 || h["mem.sysUsage"] > capKB/20 {
		t.Errorf("sysUsage %d KB of %d KB", h["mem.sysUsage"], capKB)
	}
	if total := h["mem.totalCapacity"] * 1024; total <= 0 || total >= capKB {
		t.Errorf("totalCapacity %d MB of a %d KB host", h["mem.totalCapacity"], capKB)
	}
	if parts := h["mem.reservedCapacity.vmOvhd"] + h["mem.reservedCapacity.vmkOvrhd"] + h["mem.reservedCapacity.userworld"]; parts/1024 != h["mem.reservedCapacity"] {
		t.Errorf("reservedCapacity %d MB, its parts add up to %d KB", h["mem.reservedCapacity"], parts)
	}
	if h["mem.reservedCapacity.vmOvhd"] != h["mem.overhead"] {
		t.Errorf("VM overhead reserved %d KB, overhead %d KB", h["mem.reservedCapacity.vmOvhd"], h["mem.overhead"])
	}
	if d := h["mem.totalCapacity"]*1024 - h["mem.unreserved"] - h["mem.reservedCapacity"]*1024; d < 0 || d > 1024 {
		t.Errorf("total %d MB, reserved %d MB, unreserved %d KB do not add up", h["mem.totalCapacity"], h["mem.reservedCapacity"], h["mem.unreserved"])
	}
	v := sample(vm, "mem.entitlement", "mem.consumed")
	if v["mem.consumed"] <= 0 || v["mem.entitlement"] < v["mem.consumed"] {
		t.Errorf("VM entitled to %d KB, consumes %d KB", v["mem.entitlement"], v["mem.consumed"])
	}
}
