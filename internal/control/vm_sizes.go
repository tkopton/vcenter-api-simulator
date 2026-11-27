package control

import (
	"fmt"
	"log"
	"maps"
	"sort"
	"strings"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// vmResized keeps the sizes tools gave VMs (a rightsizing action from VCF
// Operations, say) in the scenario's vmSizes, so that a rebuild or a
// restart makes those VMs that size again. The running vCenter already has
// the change and is not rebuilt.
func (c *Controller) vmResized(from *sim.Simulator) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.applied()
	if cur == nil || from == nil || cur.sim != from {
		return // a vCenter that has since been replaced
	}
	sizes := from.VMSizes()
	next := clone(cur.sc)
	var told []string
	for _, cl := range cur.topo.Clusters() {
		want, had := sizes[cl.Key], next.VMSizes(cl.Key)
		if maps.Equal(want, had) {
			continue
		}
		override(next, cl.Key).VMSizes = want
		for n, size := range want {
			if had[n] != size {
				told = append(told, fmt.Sprintf("%s to %s", vmName(cl, n), vmSizeText(size)))
			}
		}
		for n := range had {
			if _, ok := want[n]; !ok {
				told = append(told, vmName(cl, n)+" back to the size it was built with")
			}
		}
	}
	if len(told) == 0 {
		return
	}
	if err := clone(next).Validate(); err != nil {
		// what the tool did cannot be kept; the running vCenter still has it
		log.Printf("control: VM sizes not saved: %v", err)
		return
	}
	c.cur.Store(&running{version: cur.version, at: cur.at, sc: next, topo: cur.topo, sim: cur.sim})
	sort.Strings(told)
	c.addHistory("A tool resized " + strings.Join(told, ", ") + ".")
	c.save()
}

// vmName is the name of a cluster's VM by number.
func vmName(cl *topology.Cluster, n int) string {
	for _, vm := range cl.LiveVMs() {
		if vm.Index+1 == n {
			return vm.Name
		}
	}
	return fmt.Sprintf("%s VM %d", cl.Name, n)
}

func vmSizeText(s scenario.VMSize) string {
	mem := fmt.Sprintf("%d MB", s.MemoryMB)
	if s.MemoryMB%1024 == 0 {
		mem = fmt.Sprintf("%d GB", s.MemoryMB/1024)
	}
	return fmt.Sprintf("%s and %s", plural(s.CPUs, "vCPU"), mem)
}
