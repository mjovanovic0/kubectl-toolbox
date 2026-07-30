package resolve

import (
	"context"
	"strings"
	"testing"

	"github.com/mjovanovic0/kubectl-vmsnap/internal/source"
	"github.com/mjovanovic0/kubectl-vmsnap/internal/tree"
)

// The prodshape fixture mirrors a real Portworx/FlashArray cluster and pins the
// bugs that only showed up against it.
func newProdResolver(t *testing.T) *Resolver {
	t.Helper()
	d, err := source.NewDump("../../testdata/prodshape")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	return New(d, Options{Namespace: "devc-org200"})
}

// A Quantity with no unit suffix ("171798691840") must render in binary units
// like every other size, not as raw digits.
func TestUnsuffixedQuantityIsHumanised(t *testing.T) {
	r := newProdResolver(t)
	ls := lines(r.VM(context.Background(), "urt-f12", VMOptions{}))

	pvc := findLine(t, ls, "PersistentVolumeClaim/restore-6f4117e6")
	if !strings.Contains(pvc, "size=160Gi/160Gi") {
		t.Errorf("expected both sizes humanised; got: %s", pvc)
	}
	vb := findLine(t, ls, "volumeBackup[rootdisk]")
	if !strings.Contains(vb, "size=160Gi") {
		t.Errorf("captured PVC size must be humanised; got: %s", vb)
	}
	for _, l := range ls {
		if strings.Contains(l, "171798691840") {
			t.Errorf("raw byte count leaked into output: %s", l)
		}
	}
}

// VolumeRestore's json tag is persistentVolumeClaim, not
// persistentVolumeClaimName; using the wrong key printed an empty field and
// mislabelled the VolumeSnapshot as the new PVC.
func TestRestoreMappingUsesCorrectFieldNames(t *testing.T) {
	r := newProdResolver(t)
	ls := lines(r.VM(context.Background(), "urt-f12", VMOptions{}))

	l := findLine(t, ls, "restore-urt-f12-20260625-155250")
	want := "disk rootdisk: from VolumeSnapshot vmsnapshot-103e9e1b-f65a-4f3c-8a8b-e9fc48245f3e-volume-rootdisk " +
		"into new PVC restore-6f4117e6-6a9a-487f-8291-95c84e1d7811-rootdisk " +
		"via DataVolume restore-6f4117e6-6a9a-487f-8291-95c84e1d7811-rootdisk"
	if !strings.Contains(l, want) {
		t.Errorf("restore mapping wrong.\nwant substring: %s\ngot: %s", want, l)
	}
	// The old bug produced "disk rootdisk:  -> new PVC <snapshot name>".
	if strings.Contains(l, "rootdisk:  ->") {
		t.Errorf("empty PVC field regression: %s", l)
	}
}

// An incomplete restore with no Failure condition yet must still be flagged, via
// the generic fallback rather than a quoted reason.
func TestIncompleteRestoreWithoutFailureConditionWarns(t *testing.T) {
	d, err := source.NewDump("../../testdata/restorepending")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	r := New(d, Options{Namespace: "virt-prod"})
	rep := r.Snapshot(context.Background(), "pending-vm", "pending-snap")
	ls := lines(rep)

	l := findLine(t, ls, "VirtualMachineRestore/restore-pending")
	if !strings.Contains(l, "@warn") {
		t.Errorf("an incomplete restore must warn; got: %s", l)
	}
	if !strings.Contains(l, "incomplete, created") {
		t.Errorf("expected the age note; got: %s", l)
	}
	if !strings.Contains(l, "[PVC not created yet]") {
		t.Errorf("its PVC does not exist yet and should say so; got: %s", l)
	}

	var found bool
	for _, p := range rep.Problems {
		if strings.Contains(p.Message, "restore has not completed") {
			found = true
			// The target here really is running, so that is the advice to give.
			if !strings.Contains(p.Hint, "is Running") || !strings.Contains(p.Hint, "stop it") {
				t.Errorf("hint should name the running target as the blocker; got: %s", p.Hint)
			}
		}
	}
	if !found {
		t.Errorf("expected the fallback problem; got: %+v", rep.Problems)
	}
}

// When a DataVolume's declared source disagrees with what actually populated the
// PVC, say so -- otherwise you go looking for the wrong volume on the array.
func TestDataVolumeSourceDisagreementIsExplained(t *testing.T) {
	r := newProdResolver(t)
	ls := lines(r.VM(context.Background(), "urt-f12", VMOptions{}))

	l := findLine(t, ls, "spec.dataSourceRef VolumeSnapshot/vmsnapshot-103e9e1b")
	if !strings.Contains(l, "this is what populated the volume") {
		t.Errorf("expected the disagreement note; got: %s", l)
	}
	if !strings.Contains(l, "org200-t-srv25-rootdisk") {
		t.Errorf("note should name the DataVolume's declared source; got: %s", l)
	}
}

// The snapshot's origin array volume is resolved, and its absence is lineage
// rather than a fault.
func TestOriginVolumeLookupIsSoft(t *testing.T) {
	r := newProdResolver(t)
	rep := r.VM(context.Background(), "urt-f12", VMOptions{})
	ls := lines(rep)

	l := findLine(t, ls, "origin PureVolume for handle e1498acb")
	if strings.Contains(l, "@missing") || strings.Contains(l, "@warn") {
		t.Errorf("a reclaimed origin volume must not be a fault; got: %s", l)
	}
	if !strings.Contains(l, "reclaimed") {
		t.Errorf("expected an explanatory note; got: %s", l)
	}
	for _, p := range rep.Problems {
		if strings.Contains(p.Message, "e1498acb") {
			t.Errorf("origin volume absence must not be a problem: %+v", p)
		}
	}
	// The VM's live chain is healthy: every problem traces back to the failed
	// restore, either on the restore itself or on the stray PVC it created. No hop
	// of the running disk or the snapshot chain may be implicated.
	for _, p := range rep.Problems {
		expected := strings.Contains(p.Where, "VirtualMachineRestore/") ||
			strings.Contains(p.Where, "restore-0bd08e71") ||
			// The snapshot's array-side parent volume being gone is a real, separate
			// finding about the snapshot, not about the VM's live chain.
			strings.Contains(p.Where, "PureSnapshot/")
		if !expected {
			t.Errorf("unexpected problem outside the failed restore: %+v", p)
		}
	}
}

// The primary array lookup must still hard-fail, unlike the soft origin lookup.
func TestPrimaryArrayLookupStillReportsMissing(t *testing.T) {
	r := newBrokenResolver(t)
	rep := r.Snapshot(context.Background(), "vm-bad", "snap-bad")
	if !strings.Contains(problemMessages(rep), "no PureSnapshot matches CSI handle") {
		t.Errorf("primary lookup must remain a reported problem; got:\n%s", problemMessages(rep))
	}
}

// A Succeeded DataVolume routinely carries a bare Running=False condition. Making
// empty conditions visible must not turn that into noise on a healthy disk.
func TestFinishedDataVolumeConditionsAreNotNoise(t *testing.T) {
	r := newProdResolver(t)
	ls := lines(r.VM(context.Background(), "urt-f12", VMOptions{}))

	dv := findLine(t, ls, "DataVolume/restore-6f4117e6")
	if strings.Contains(dv, "no reason given") {
		t.Errorf("a Succeeded DataVolume must not report contentless conditions; got: %s", dv)
	}
	if !strings.Contains(dv, "dvPhase=Succeeded") {
		t.Errorf("expected the phase attr; got: %s", dv)
	}
}

// One stuck restore must produce exactly one problem: the Failure condition
// explains it, so the generic "did not complete" guess is redundant.
func TestFailedRestoreReportsOneProblemNotTwo(t *testing.T) {
	r := newProdResolver(t)
	rep := r.VM(context.Background(), "urt-f12", VMOptions{})

	// The point is that one fault is not reported twice: the Failure message must
	// appear, and the generic "has not completed" guess must not accompany it.
	// A separate orphaned-PVC finding is a different fault and may coexist.
	var failures, generic int
	for _, p := range rep.Problems {
		if !strings.Contains(p.Where, "restore-urt-f12-snap-1784811827447") {
			continue
		}
		if strings.Contains(p.Message, "restore failed after") {
			failures++
			if !strings.Contains(p.Message, "power off the target VM") {
				t.Errorf("problem should carry the real reason; got: %s", p.Message)
			}
			if !strings.Contains(p.Hint, "recreate this VirtualMachineRestore") {
				t.Errorf("hint should say how to retry; got: %s", p.Hint)
			}
		}
		if strings.Contains(p.Message, "restore has not completed") {
			generic++
		}
	}
	if failures != 1 {
		t.Errorf("want exactly 1 failure problem, got %d: %+v", failures, rep.Problems)
	}
	if generic != 0 {
		t.Errorf("the generic incomplete warning must not accompany a Failure condition: %+v", rep.Problems)
	}
	if done := findLine(t, lines(rep), "restore-urt-f12-20260625-155250"); strings.Contains(done, "@warn") || strings.Contains(done, "@error") {
		t.Errorf("the completed restore must not be flagged; got: %s", done)
	}
	if rep.WorstState() != tree.StateError {
		t.Errorf("WorstState = %s, want error", stateName(rep.WorstState()))
	}

	// Ready=False restates Failure verbatim and must not be printed twice.
	ls := lines(rep)
	l := findLine(t, ls, "restore-urt-f12-snap-1784811827447")
	if strings.Count(l, "Restore target failed to be ready") != 1 {
		t.Errorf("the failure message should appear once; got: %s", l)
	}
}

// Each restored PVC is checked for existence: present, not-yet-created, or gone.
func TestRestoredPVCExistenceIsChecked(t *testing.T) {
	r := newProdResolver(t)
	ls := lines(r.VM(context.Background(), "urt-f12", VMOptions{}))

	if l := findLine(t, ls, "restore-urt-f12-20260625-155250"); !strings.Contains(l, "[PVC exists]") {
		t.Errorf("completed restore's PVC exists and should say so; got: %s", l)
	}
	// This restore's PVC does now exist despite the failure -- flagged as an
	// orphan. The "not created yet" branch is covered by the restorepending fixture.
	if l := findLine(t, ls, "restore-urt-f12-snap-1784811827447"); !strings.Contains(l, "phase Pending, owned by") {
		t.Errorf("failed restore's stray PVC must be flagged; got: %s", l)
	}
	// deletedDataVolumes explains where the original disk went.
	if l := findLine(t, ls, "restore-urt-f12-20260625-155250"); !strings.Contains(l, "deleted DataVolume during restore: urt-f12-rootdisk") {
		t.Errorf("expected the deleted DataVolume to be reported; got: %s", l)
	}
}

// A snapshot whose source PVC is gone is the last remaining copy, and deleting it
// discards the array snapshot -- worth saying out loud.
func TestLastCopyWarningOnDeletedSourcePVC(t *testing.T) {
	r := newProdResolver(t)
	ls := lines(r.VM(context.Background(), "urt-f12", VMOptions{}))
	vb := findLine(t, ls, "volumeBackup[rootdisk]")
	if !strings.Contains(vb, "only copy of that disk") {
		t.Errorf("expected a last-copy note; got: %s", vb)
	}
}

// A failed restore that has nonetheless created its PVC must be reported, stating
// both whether array capacity is allocated and whether anything will reclaim it.
func TestStrayPVCFromFailedRestoreIsReported(t *testing.T) {
	r := newProdResolver(t)
	rep := r.VM(context.Background(), "urt-f12", VMOptions{})
	ls := lines(rep)

	l := findLine(t, ls, "restore-urt-f12-snap-1784811827447")
	if !strings.Contains(l, "phase Pending, owned by VirtualMachine/urt-f12") {
		t.Errorf("expected phase and owner on the stray PVC; got: %s", l)
	}
	if !strings.Contains(l, "targetReadinessPolicy=WaitGracePeriod") {
		t.Errorf("the policy governing the timeout must be shown; got: %s", l)
	}

	// Whether array capacity is actually consumed must be stated explicitly: an
	// unbound PVC has no array volume, and confusing the two sends you hunting a
	// volume that was never provisioned.
	var found bool
	for _, p := range rep.Problems {
		if strings.Contains(p.Message, "has already created PVC") {
			found = true
			if !strings.Contains(p.Message, "no array volume has been provisioned") {
				t.Errorf("must say no capacity is allocated for an unbound PVC; got: %s", p.Message)
			}
			if !strings.Contains(p.Hint, "will not remove it") {
				t.Errorf("hint should say deleting the restore leaves it; got: %s", p.Hint)
			}
		}
	}
	if !found {
		t.Errorf("expected a stray-PVC problem; got: %+v", rep.Problems)
	}

	// The stray PVC is resolved, and its snapshot wiring shown, without diving
	// into the snapshot chain a second time.
	pvc := findLine(t, ls, "PersistentVolumeClaim/restore-0bd08e71")
	if !strings.Contains(pvc, "never bound to a PersistentVolume") {
		t.Errorf("expected the unbound note; got: %s", pvc)
	}
	// The WaitForFirstConsumer explanation is wrong here -- no consumer is coming --
	// so it must be replaced, not merely repeated.
	if !strings.Contains(pvc, "will never bind") {
		t.Errorf("expected the abandoned-restore explanation; got: %s", pvc)
	}
	for _, p := range rep.Problems {
		if strings.Contains(p.Hint, "WaitForFirstConsumer") {
			t.Errorf("must not offer the WFFC explanation for a stray restore PVC: %+v", p)
		}
	}
}

// A restored PVC's dataSourceRef must be shown and fully resolved: whether the
// PVC is wired to its snapshot, and which array object that is, is the point of
// looking. This replaces an earlier behaviour that stopped at the reference.
func TestStrayPVCDataSourceRefIsFullyResolved(t *testing.T) {
	r := newProdResolver(t)
	rep := r.VM(context.Background(), "urt-f12", VMOptions{})
	ls := lines(rep)

	// Find the ref under the stray PVC specifically, not the healthy disk's.
	var strayRef *tree.Node
	var walk func(*tree.Node)
	walk = func(n *tree.Node) {
		if strings.Contains(n.Label, "PersistentVolumeClaim/restore-0bd08e71") {
			for _, c := range n.Children {
				if strings.Contains(c.Label, "spec.dataSourceRef") {
					strayRef = c
				}
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(rep.Root)
	if strayRef == nil {
		t.Fatalf("the stray PVC has no dataSourceRef node:\n%s", strings.Join(ls, "\n"))
	}
	if len(strayRef.Children) == 0 {
		t.Fatalf("the ref must be resolved, not merely named: %s", strayRef.Label)
	}
	// And it must reach the array object.
	var sub []string
	flatten(strayRef, &sub)
	if !strings.Contains(strings.Join(sub, "\n"), "PureSnapshot/") {
		t.Errorf("the stray PVC's snapshot chain must reach the array:\n%s", strings.Join(sub, "\n"))
	}
}

func TestNodeSeverityIsNeverDowngraded(t *testing.T) {
	r := newProdResolver(t)
	ls := lines(r.VM(context.Background(), "urt-f12", VMOptions{}))

	l := findLine(t, ls, "restore-urt-f12-snap-1784811827447")
	if !strings.Contains(l, "@error") {
		t.Errorf("failed restore must remain @error after the orphan warning; got: %s", l)
	}
}

// Escalate raises but never lowers.
func TestEscalateOnlyRaises(t *testing.T) {
	n := tree.New("x").Escalate(tree.StateError)
	n.Escalate(tree.StateWarn)
	if n.State != tree.StateError {
		t.Errorf("state was downgraded to %s", stateName(n.State))
	}
	n.Escalate(tree.StateMissing)
	if n.State != tree.StateMissing && n.State != tree.StateError {
		t.Errorf("unexpected state %s", stateName(n.State))
	}
}

// Chains are shown in full by default. Nothing is collapsed unless asked for:
// a reader following one branch must not have to hunt for it elsewhere.
func TestChainsAreCompleteByDefault(t *testing.T) {
	r := newProdResolver(t)
	ls := lines(r.VM(context.Background(), "urt-f12", VMOptions{}))

	for _, l := range ls {
		if strings.Contains(l, "already shown") || strings.Contains(l, "shown elsewhere") {
			t.Errorf("default output must not collapse anything; got: %s", l)
		}
	}

	// The array object is reached from every branch that leads to it.
	var arrival int
	for _, l := range ls {
		if strings.Contains(l, "PureSnapshot/9c1b16f7-4f59-b969-22df-a1e5e697096c") {
			arrival++
		}
	}
	if arrival < 3 {
		t.Errorf("want the array snapshot resolved on all 3 branches, got %d:\n%s", arrival, strings.Join(ls, "\n"))
	}
}

// --trim-repeats collapses later repeats while keeping the first in full.
func TestTrimRepeatsCollapsesOnlyRepeats(t *testing.T) {
	d, err := source.NewDump("../../testdata/prodshape")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	r := New(d, Options{Namespace: "devc-org200", TrimRepeats: true})
	ls := lines(r.VM(context.Background(), "urt-f12", VMOptions{}))

	var full, trimmed int
	for _, l := range ls {
		if strings.Contains(l, "PureSnapshot/9c1b16f7-4f59-b969-22df-a1e5e697096c") {
			full++
		}
		if strings.Contains(l, "already shown above") {
			trimmed++
		}
	}
	if full != 1 {
		t.Errorf("the chain should be rendered in full exactly once, got %d", full)
	}
	if trimmed < 1 {
		t.Errorf("expected repeats to be collapsed; got none")
	}
	// Even a collapsed node keeps its own attributes, so it is never a dead end.
	l := findLine(t, ls, "already shown above")
	if !strings.Contains(l, "ready=true") {
		t.Errorf("a collapsed node must still carry its attrs; got: %s", l)
	}
}

// The collapse note must never claim something was "shown above" when it wasn't.
func TestTrimRepeatsNeverLies(t *testing.T) {
	d, err := source.NewDump("../../testdata/prodshape")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	r := New(d, Options{Namespace: "devc-org200", TrimRepeats: true})
	// This entry point renders the snapshot chain exactly once.
	ls := lines(r.VolumeSnapshotChain(context.Background(), "vmsnapshot-103e9e1b-f65a-4f3c-8a8b-e9fc48245f3e-volume-rootdisk"))

	for _, l := range ls {
		if strings.Contains(l, "already shown") {
			t.Errorf("nothing was repeated, so nothing may claim to be: %s", l)
		}
	}
	findLine(t, ls, "PureSnapshot/9c1b16f7-4f59-b969-22df-a1e5e697096c")
}
