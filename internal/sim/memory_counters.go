package sim

// Memory counters a real ESXi host or VM has beyond the ones the main model
// answers. Without the host's reservation and VMkernel counters a capacity
// tool cannot work out a host's memory utilization (VCF Operations builds it
// from the VMs' utilization plus reservations and overhead), so cluster
// memory demand stays empty. Which object has which counter follows the
// vSphere API reference (memory counters); "aggregate" counters also roll up
// to clusters and datacenters.

// memoryEntities says where each added counter is served.
var memoryEntities = map[string]map[string]bool{}

func init() {
	host := []string{"HostSystem", "ClusterComputeResource", "Datacenter"}
	vm := []string{"VirtualMachine"}
	both := []string{"HostSystem", "VirtualMachine", "ClusterComputeResource", "Datacenter"}
	for kinds, names := range map[*[]string][]string{
		&host: {"mem.sysUsage", "mem.heap", "mem.heapfree", "mem.lowfreethreshold", "mem.totalCapacity", "mem.unreserved",
			"mem.reservedCapacity", "mem.reservedCapacity.vm", "mem.reservedCapacity.vmOvhd", "mem.reservedCapacity.vmkOvrhd",
			"mem.reservedCapacity.userworld", "mem.reservedCapacityPct", "mem.sharedcommon", "mem.consumed.userworlds",
			"mem.capacity.usage.vmkOvrhd", "mem.capacity.usage.userworld", "mem.swapunreserved", "mem.llSwapIn", "mem.llSwapOut",
			"mem.vmfs.pbc.capMissRatio", "mem.vmfs.pbc.overhead", "mem.vmfs.pbc.size", "mem.vmfs.pbc.sizeMax",
			"mem.vmfs.pbc.workingSet", "mem.vmfs.pbc.workingSetMax"},
		&vm:   {"mem.entitlement", "mem.overheadMax", "mem.overheadTouched", "mem.vmmemctltarget", "mem.zipped", "mem.zipSaved"},
		&both: {"mem.latency", "mem.activewrite", "mem.swapin", "mem.swapout", "mem.llSwapInRate", "mem.llSwapOutRate", "mem.llSwapUsed"},
	} {
		for _, name := range names {
			memoryEntities[name] = map[string]bool{}
			for _, t := range *kinds {
				memoryEntities[name][t] = true
			}
		}
	}
}

// memoryValue answers the added memory counters; handled is false for any
// other counter, which the main model then answers.
func memoryValue(name string, n *workloadNode, w workload) (v int64, ok, handled bool) {
	where, known := memoryEntities[name]
	if !known {
		return 0, false, false
	}
	if !where[n.ref.Type] {
		return 0, false, true
	}
	// a host's memory, in KB: the VMkernel and its services take a fixed
	// share, the VMs' overhead is reserved, and the rest is free to reserve
	capKB := w.memCap * 1024
	sysKB := capKB * 12 / 1000         // VMkernel core: drivers, heap and the like
	userKB := capKB * 8 / 1000         // vSphere services (hostd, vpxa) as userworlds
	overheadKB := w.memory * 1024 / 40 // matches mem.overhead
	totalMB := (capKB - sysKB) / 1024
	reservedKB := overheadKB + sysKB/4 + userKB // VMs reserve nothing themselves
	switch name {
	case "mem.sysUsage", "mem.capacity.usage.vmkOvrhd":
		return sysKB, true, true
	case "mem.heap":
		return sysKB / 6, true, true
	case "mem.heapfree":
		return sysKB / 15, true, true
	case "mem.lowfreethreshold":
		return capKB / 100, true, true
	case "mem.totalCapacity":
		return totalMB, true, true
	case "mem.reservedCapacity":
		return reservedKB / 1024, true, true
	case "mem.reservedCapacity.vm":
		return 0, true, true
	case "mem.reservedCapacity.vmOvhd":
		return overheadKB, true, true
	case "mem.reservedCapacity.vmkOvrhd":
		return sysKB / 4, true, true
	case "mem.reservedCapacity.userworld", "mem.consumed.userworlds", "mem.capacity.usage.userworld":
		return userKB, true, true
	case "mem.reservedCapacityPct":
		return reservedKB * 10000 / max(1, totalMB*1024), true, true // hundredths of a percent
	case "mem.unreserved":
		return max(0, totalMB*1024-reservedKB), true, true
	case "mem.sharedcommon":
		return w.memory * 1024 / 90, true, true
	case "mem.entitlement":
		// at least what the VM consumes, never more than it is configured with
		return min(capKB, w.memory*1024*11/10), true, true
	case "mem.overheadMax":
		return overheadKB * 2, true, true
	case "mem.overheadTouched":
		return overheadKB * 6 / 10, true, true
	case "mem.vmmemctltarget":
		return w.balloon * 1024, true, true
	case "mem.activewrite":
		return w.activeMemory * 1024 / 4, true, true
	case "mem.swapin":
		return w.swapped * 1024 / 2, true, true
	case "mem.swapout":
		return w.swapped * 1024, true, true
	case "mem.latency":
		// time spent waiting for swapped memory, in hundredths of a percent
		if w.swapped > 0 {
			return 150, true, true
		}
		return 0, true, true
	}
	// host cache swap, VMFS pointer block cache, compression and the swap
	// left unreserved: present on a real host or VM, idle here
	return 0, true, true
}
