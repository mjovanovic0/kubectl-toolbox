package resolve

import (
	"context"
	"strings"
	"testing"

	"github.com/mjovanovic0/kubectl-vmsnap/internal/tree"
)

func TestVMCombinesDisksAndSnapshots(t *testing.T) {
	r := newFixtureResolver(t)
	rep := r.VM(context.Background(), "win-db-01", VMOptions{})
	ls := lines(rep)

	// Both halves must be present under their headings, with counts.
	findLine(t, ls, "disks (5)")
	findLine(t, ls, "snapshots (1)")

	// Disk side reaches PureVolume, snapshot side reaches PureSnapshot.
	findLine(t, ls, "PureVolume/purevolume-win-db-01-root")
	findLine(t, ls, "PureSnapshot/puresnapshot-win-db-01-root-snap-8f3a")

	// And the problems from both halves are merged into one list.
	msgs := problemMessages(rep)
	for _, want := range []string{
		`PersistentVolumeClaim "win-db-01-data" does not exist`,        // disk side
		`VolumeSnapshot "vmsnapshot-8f3a-vol-datadisk" does not exist`, // snapshot side
	} {
		if !strings.Contains(msgs, want) {
			t.Errorf("expected merged problem %q; got:\n%s", want, msgs)
		}
	}
	if got := rep.WorstState(); got != tree.StateMissing {
		t.Errorf("WorstState = %s, want missing", stateName(got))
	}
}

func TestVMHalvesCanBeSkipped(t *testing.T) {
	r := newFixtureResolver(t)

	noSnaps := lines(r.VM(context.Background(), "win-db-01", VMOptions{SkipSnapshots: true}))
	findLine(t, noSnaps, "disks (5)")
	for _, l := range noSnaps {
		if strings.Contains(l, "snapshots (") {
			t.Errorf("--no-snapshots must omit the snapshots section; got: %s", l)
		}
	}

	noDisks := lines(r.VM(context.Background(), "win-db-01", VMOptions{SkipDisks: true}))
	findLine(t, noDisks, "snapshots (1)")
	for _, l := range noDisks {
		if strings.Contains(l, "disks (") {
			t.Errorf("--no-disks must omit the disks section; got: %s", l)
		}
	}
}

// The total count must stay honest when --latest truncates, and the omission
// must be stated rather than silently narrowing the report.
func TestVMLatestReportsWhatItDropped(t *testing.T) {
	r := newFixtureResolver(t)
	rep := r.VM(context.Background(), "win-db-01", VMOptions{Latest: 1, SkipDisks: true})
	ls := lines(rep)
	l := findLine(t, ls, "snapshots (1)")
	if strings.Contains(l, "not resolved") {
		t.Errorf("nothing should be dropped when total <= latest; got: %s", l)
	}
}

func TestVMReportsSnapshotsWhenVMIsGone(t *testing.T) {
	r := newBrokenResolver(t)
	rep := r.VM(context.Background(), "vm-bad", VMOptions{})
	ls := lines(rep)

	vm := findLine(t, ls, "VirtualMachine/vm-bad")
	if !strings.Contains(vm, "outlived") {
		t.Errorf("expected a note that snapshots outlived the VM; got: %s", vm)
	}
	// The snapshot chain must still be resolved -- this is the main reason to
	// reach for the tool.
	findLine(t, ls, "snapshots (1)")
	findLine(t, ls, "VirtualMachineSnapshotContent/content-bad")
}

func TestVMWithNoSnapshotsSaysSo(t *testing.T) {
	r := newFixtureResolver(t)
	// win2022-abc123 is a PVC, not a VM, so no snapshot sources it.
	rep := r.VM(context.Background(), "no-such-vm", VMOptions{})
	ls := lines(rep)
	l := findLine(t, ls, "snapshots (0)")
	if !strings.Contains(l, "no VirtualMachineSnapshot has this VM") {
		t.Errorf("expected an explicit empty note; got: %s", l)
	}
}
