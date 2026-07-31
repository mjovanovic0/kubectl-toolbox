package resolve

import (
	"context"
	"strings"
	"testing"

	"github.com/mjovanovic0/kubectl-toolbox/internal/source"
	"github.com/mjovanovic0/kubectl-toolbox/internal/tree"
)

func newBrokenResolver(t *testing.T) *Resolver {
	t.Helper()
	// The broken fixture is a `kind: List` capture, so loading it at all proves
	// List unwrapping works.
	d, err := source.NewDump("../../testdata/broken")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	return New(d, Options{Namespace: "virt-prod"})
}

func problemMessages(rep *tree.Report) string {
	var sb strings.Builder
	for _, p := range rep.Problems {
		sb.WriteString(p.Message + "\n")
	}
	return sb.String()
}

func TestBrokenChainDetectsAllInconsistencies(t *testing.T) {
	r := newBrokenResolver(t)
	rep := r.Snapshot(context.Background(), "vm-bad", "snap-bad")
	msgs := problemMessages(rep)

	for _, want := range []string{
		// snapshot itself
		`snapshot phase is "InProgress"`,
		"snapshot is not readyToUse",
		"timed out waiting for volume snapshots",
		// content <-> snapshot pairing
		"snap-somethingelse",
		// volumeBackup PVC vs VolumeSnapshot source disagreement
		"some-unrelated-pvc",
		// crossed volumeSnapshotRef
		"vs-someone-else",
		// stale status entry with no backup
		"vs-orphaned-status-entry",
		// array object not found for an existing handle
		"no PureSnapshot matches CSI handle",
	} {
		if !strings.Contains(msgs, want) {
			t.Errorf("expected a problem mentioning %q; got:\n%s", want, msgs)
		}
	}
}

func TestBrokenChainReportsUnmatchedHandleWithoutClaimingSuccess(t *testing.T) {
	r := newBrokenResolver(t)
	rep := r.Snapshot(context.Background(), "vm-bad", "snap-bad")
	ls := lines(rep)

	// The handle must still be printed even though no array CR matched, because
	// it is what you take to the array console.
	vsc := findLine(t, ls, "VolumeSnapshotContent/snapcontent-bad")
	if !strings.Contains(vsc, "snapshotHandle=pure-fa1::vm-bad-root.snap-orphan") {
		t.Errorf("expected the handle to be reported; got: %s", vsc)
	}
	arr := findLine(t, ls, "PureSnapshot for handle")
	if !strings.Contains(arr, "@missing") {
		t.Errorf("unmatched array lookup must be missing; got: %s", arr)
	}
	// It must say what it scanned, so the result is falsifiable.
	if !strings.Contains(arr, "scanned 1 PureSnapshot objects") {
		t.Errorf("expected the scan to be reported; got: %s", arr)
	}

	if got := rep.WorstState(); got != tree.StateError && got != tree.StateMissing {
		t.Errorf("WorstState = %s, want error or missing", stateName(got))
	}
}

func TestVMAbsenceIsWarnNotFailure(t *testing.T) {
	r := newBrokenResolver(t)
	rep := r.Snapshot(context.Background(), "vm-bad", "snap-bad")
	ls := lines(rep)
	// vm-bad does not exist in the fixture; the chain must still resolve.
	vm := findLine(t, ls, "VirtualMachine/vm-bad")
	if !strings.Contains(vm, "@warn") {
		t.Errorf("absent VM should warn, not abort; got: %s", vm)
	}
	findLine(t, ls, "VirtualMachineSnapshotContent/content-bad")
}
