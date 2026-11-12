package alerts

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// raid is an array as LocalStorage answers it; members are "disk:state".
func raid(name, level, state string, disks, missing int, members ...string) raidArray {
	a := raidArray{Name: name, Level: level, State: state, Disks: disks, Missing: missing}
	for _, m := range members {
		disk, s, _ := strings.Cut(m, ":")
		a.Members = append(a.Members, raidMember{Disk: disk, State: s})
	}
	return a
}

func TestWhatTheArraysShow(t *testing.T) {
	for _, tc := range []struct {
		name           string
		arrays         []raidArray
		raise, resolve []string
		raiseSentences []string
	}{
		{"a healthy array clears both", []raidArray{raid("md0", "raid1", "ok", 2, 0, "sdb:active", "sdc:active")},
			nil, []string{"raid:md0:degraded", "raid:md0:inactive"}, nil},
		{"a check or a resync is not a fault", []raidArray{raid("md0", "raid1", "checking", 2, 0), raid("md1", "raid5", "resyncing", 3, 0)},
			nil, []string{"raid:md0:degraded", "raid:md0:inactive", "raid:md1:degraded", "raid:md1:inactive"}, nil},
		{"a disk gone", []raidArray{raid("md0", "raid1", "degraded", 2, 1, "sdb:active")},
			[]string{"raid:md0:degraded"}, []string{"raid:md0:inactive"},
			[]string{"RAID md0 (RAID1) is degraded: 1 of its 2 disks is missing. Replace it before another disk fails."}},
		{"two disks gone", []raidArray{raid("md0", "raid6", "degraded", 4, 2)},
			[]string{"raid:md0:degraded"}, []string{"raid:md0:inactive"},
			[]string{"RAID md0 (RAID6) is degraded: 2 of its 4 disks are missing. Replace them before another disk fails."}},
		{"a disk failed", []raidArray{raid("md0", "raid1", "degraded", 2, 1, "sdb:active", "sdc:faulty")},
			[]string{"raid:md0:degraded"}, []string{"raid:md0:inactive"},
			[]string{"RAID md0 (RAID1) is degraded: disk sdc has failed. Replace it before another disk fails."}},
		{"a spare stood in for a failed disk", []raidArray{raid("md0", "raid1", "degraded", 2, 0, "sdb:active", "sdc:faulty", "sdd:active")},
			[]string{"raid:md0:degraded"}, []string{"raid:md0:inactive"},
			[]string{"Disk sdc has failed in RAID md0 (RAID1); a spare took its place. Replace the failed disk."}},
		{"a rebuild is still degraded", []raidArray{raid("md0", "raid5", "rebuilding", 3, 1, "sdb:active", "sdc:active", "sdd:rebuilding")},
			[]string{"raid:md0:degraded"}, []string{"raid:md0:inactive"},
			[]string{"RAID md0 (RAID5) is degraded: 1 of its 3 disks is missing. Replace it before another disk fails."}},
		{"an array that does not run", []raidArray{raid("md0", "raid1", "inactive", 2, 0)},
			[]string{"raid:md0:inactive"}, nil,
			[]string{"RAID md0 (RAID1) is not running: its data cannot be reached."}},
		{"a btrfs member gone has a name, no disk", []raidArray{{Name: "pool", Level: "raid1", State: "degraded", Disks: 2, Missing: 1,
			Members: []raidMember{{Name: "vdb", Disk: "vdb", State: "active"}, {Name: "devid 2", State: "faulty"}}}},
			[]string{"raid:pool:degraded"}, []string{"raid:pool:inactive"},
			[]string{"RAID pool (RAID1) is degraded: disk devid 2 has failed. Replace it before another disk fails."}},
		{"a btrfs that does not mount has no level", []raidArray{raid("pool", "", "inactive", 2, 1, "vdb:active")},
			[]string{"raid:pool:inactive"}, nil,
			[]string{"RAID pool is not running: its data cannot be reached."}},
		{"no array, nothing to say", nil, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raise, resolve := raidAlerts(tc.arrays)
			if !reflect.DeepEqual(keys(raise), tc.raise) || !reflect.DeepEqual(keys(resolve), tc.resolve) {
				t.Fatalf("raise %v, resolve %v; want %v, %v", keys(raise), keys(resolve), tc.raise, tc.resolve)
			}
			var sentences []string
			for _, a := range raise {
				sentences = append(sentences, a.sentence)
				if a.category != Disks {
					t.Fatalf("%s is in %s, want disks", a.key, a.category)
				}
			}
			if !reflect.DeepEqual(sentences, tc.raiseSentences) {
				t.Fatalf("sentences %q, want %q", sentences, tc.raiseSentences)
			}
		})
	}
}

// A poll asks LocalStorage; a rebuild that ends says the array is complete
// again, and a LocalStorage too old to answer says nothing.
func TestPollingTheArrays(t *testing.T) {
	h, o, _ := newHub(t)
	runtimePath := t.TempDir()
	h.RuntimePath = func() string { return runtimePath }
	f := newLocalStorage(t, runtimePath)
	poll := func(arrays []raidArray) {
		t.Helper()
		f.mu.Lock()
		f.raid = arrays
		f.mu.Unlock()
		h.pollRAID(context.Background())
		h.sending.Wait()
	}
	address := "\nhttp://192.168.1.20"

	poll(nil) // an older LocalStorage: 404
	poll([]raidArray{raid("md0", "raid1", "degraded", 2, 1, "sdb:active")})
	poll([]raidArray{raid("md0", "raid1", "rebuilding", 2, 1, "sdb:active", "sdc:rebuilding")})
	poll([]raidArray{raid("md0", "raid1", "ok", 2, 0, "sdb:active", "sdc:active")})

	got := o.texts(phone)
	want := []string{
		"RAID md0 (RAID1) is degraded: 1 of its 2 disks is missing. Replace it before another disk fails." + address,
		"Resolved: RAID md0 (RAID1) is complete again." + address,
	}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
}

func counted(n uint64) *uint64 { return &n }

// withErrors is an array whose members ("disk") report these error counters.
func withErrors(errors map[string]uint64) []raidArray {
	a := raidArray{Name: "pool", Level: "raid1", State: "ok", Disks: len(errors)}
	for _, disk := range []string{"vdb", "vdc"} {
		if n, ok := errors[disk]; ok {
			a.Members = append(a.Members, raidMember{Name: disk, Disk: disk, State: "active", Errors: counted(n)})
		}
	}
	return []raidArray{a}
}

func TestErrorCountersThatGrow(t *testing.T) {
	// the first poll only records: a box that carried 3 old errors for years says nothing
	raise, counts := raidErrorAlerts(nil, withErrors(map[string]uint64{"vdb": 0, "vdc": 3}))
	if len(raise) != 0 || counts["pool:vdc"] != 3 {
		t.Fatalf("first poll: %v %v", raise, counts)
	}
	raise, counts = raidErrorAlerts(counts, withErrors(map[string]uint64{"vdb": 0, "vdc": 75883}))
	want := "Disk vdc of RAID pool (RAID1) reported 75880 new read, write or checksum errors. Check the disk, and check the array."
	if len(raise) != 1 || raise[0].key != "raid:pool:vdc:errors" || raise[0].sentence != want || raise[0].category != Disks {
		t.Fatalf("grown: %+v", raise)
	}
	// unchanged: nothing new to tell
	if raise, _ = raidErrorAlerts(counts, withErrors(map[string]uint64{"vdb": 0, "vdc": 75883})); len(raise) != 0 {
		t.Fatalf("unchanged: %+v", raise)
	}
	// a LocalStorage that does not report counters (md) says nothing, and records nothing
	md := []raidArray{raid("md0", "raid1", "ok", 2, 0, "sdb:active", "sdc:active")}
	if raise, counts = raidErrorAlerts(counts, md); len(raise) != 0 || len(counts) != 0 {
		t.Fatalf("no counters: %+v %v", raise, counts)
	}
}

// A poll tells errors that grew between two polls, once.
func TestPollingTheErrorCounters(t *testing.T) {
	h, o, _ := newHub(t)
	runtimePath := t.TempDir()
	h.RuntimePath = func() string { return runtimePath }
	f := newLocalStorage(t, runtimePath)
	poll := func(arrays []raidArray) {
		t.Helper()
		f.mu.Lock()
		f.raid = arrays
		f.mu.Unlock()
		h.pollRAID(context.Background())
		h.sending.Wait()
	}
	poll(withErrors(map[string]uint64{"vdb": 0, "vdc": 0}))
	poll(withErrors(map[string]uint64{"vdb": 0, "vdc": 2}))
	poll(withErrors(map[string]uint64{"vdb": 0, "vdc": 2}))

	got := o.texts(phone)
	want := []string{"Disk vdc of RAID pool (RAID1) reported 2 new read, write or checksum errors. Check the disk, and check the array.\nhttp://192.168.1.20"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
}
