package scenario

import (
	"strings"
	"testing"
)

// vms.rightsizing is read from a scenario file, merged from defaults into a
// cluster override, and rejected when a VM would need two roles or a share
// or age is out of range.
func TestRightsizingParseMergeAndValidate(t *testing.T) {
	s, err := Parse([]byte(`seed: rs
datacenters:
  - name: dc-01
    clusters: 2
defaults:
  hosts: 3
  host: {threads: 4, domWorlds: 0, vsanPnics: 1, claimedDevices: 1, localDevices: 0}
  vms:
    perHost: 2
    disksPerVM: 1
    rightsizing: {idle: 10, poweredOff: 5, oversized: 20, undersized: 10, oldSnapshots: 10, snapshotAgeDays: {min: 30, max: 60}}
clusters:
  - key: dc-01/cluster-02
    vms: {rightsizing: {undersized: 50}}
    vmRoles: {3: poweredOff, 4: normal}
`))
	if err != nil {
		t.Fatal(err)
	}
	one := s.Resolve("dc-01/cluster-01", ClusterSpec{}).VMs.Rightsizing
	if one == nil || one.Idle != 10 || one.Oversized != 20 || one.SnapshotAge() != (Range{Min: 30, Max: 60}) {
		t.Fatalf("cluster-01 rightsizing %+v", one)
	}
	two := s.Resolve("dc-01/cluster-02", ClusterSpec{}).VMs.Rightsizing
	if two == nil || two.Undersized != 50 || two.Idle != 0 || two.SnapshotAge() != (Range{Min: 20, Max: 120}) {
		t.Fatalf("cluster-02 rightsizing %+v: an override replaces the defaults' shares as a whole", two)
	}
	if got := s.VMRoles("dc-01/cluster-02"); got[3] != RolePoweredOff || got[4] != RoleNormal {
		t.Fatalf("vmRoles %v", got)
	}

	for name, body := range map[string]string{
		"over 100":     `{idle: 60, oversized: 50}`,
		"negative":     `{idle: -1}`,
		"age backward": `{oldSnapshots: 5, snapshotAgeDays: {min: 50, max: 10}}`,
		"age zero":     `{oldSnapshots: 5, snapshotAgeDays: {min: 0, max: 10}}`,
	} {
		_, err := Parse([]byte("seed: rs\ndatacenters: [{name: dc-01, clusters: 1}]\ndefaults:\n  hosts: 3\n  host: {threads: 4, domWorlds: 0, vsanPnics: 1, claimedDevices: 1, localDevices: 0}\n  vms: {perHost: 1, disksPerVM: 1, rightsizing: " + body + "}\n"))
		if err == nil || !strings.Contains(err.Error(), "rightsizing") {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err = Parse([]byte("seed: rs\ndatacenters: [{name: dc-01, clusters: 1}]\ndefaults:\n  hosts: 3\n  host: {threads: 4, domWorlds: 0, vsanPnics: 1, claimedDevices: 1, localDevices: 0}\n  vms: {perHost: 1, disksPerVM: 1}\nclusters:\n  - key: dc-01/cluster-01\n    vmRoles: {1: sleepy}\n"))
	if err == nil || !strings.Contains(err.Error(), "vmRoles") {
		t.Errorf("unknown role: %v", err)
	}
}

// vmSizes keeps sizes a tool gave VMs; a size no vSphere VM can have is
// an error.
func TestVMSizesAreChecked(t *testing.T) {
	for _, tc := range []struct {
		size VMSize
		n    int
		ok   bool
	}{
		{VMSize{CPUs: 4, MemoryMB: 8192}, 3, true},
		{VMSize{CPUs: 0, MemoryMB: 8192}, 3, false},
		{VMSize{CPUs: 4, MemoryMB: 2}, 3, false},
		{VMSize{CPUs: MaxVMCPUs + 1, MemoryMB: 8192}, 3, false},
		{VMSize{CPUs: 4, MemoryMB: 8192}, 0, false},
	} {
		s := Phase1("sizes")
		s.Clusters = []ClusterOverride{{Key: "dc-01/cluster-01", VMSizes: map[int]VMSize{tc.n: tc.size}}}
		err := s.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("VM %d at %+v: %v", tc.n, tc.size, err)
		}
		if tc.ok && s.VMSizes("dc-01/cluster-01")[tc.n] != tc.size {
			t.Errorf("VMSizes: %v", s.VMSizes("dc-01/cluster-01"))
		}
	}
}
