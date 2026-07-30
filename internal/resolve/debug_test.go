package resolve

import (
	"context"
	"strings"
	"testing"

	"github.com/mjovanovic0/kubectl-vmstorage/internal/source"
	"github.com/mjovanovic0/kubectl-vmstorage/internal/tree"
)

func dumpResolver(t *testing.T, dir, ns string, opt Options) *Resolver {
	t.Helper()
	d, err := source.NewDump(dir)
	if err != nil {
		t.Fatalf("loading %s: %v", dir, err)
	}
	opt.Namespace = ns
	return New(d, opt)
}

// Events are the first thing a human checks, and they are attached to the object
// that is actually unhealthy.
func TestEventsAttachToUnhealthyObjects(t *testing.T) {
	r := dumpResolver(t, "../../testdata/fixture", "virt-prod", Options{})
	ls := lines(r.Disks(context.Background(), "win-db-01"))

	l := findLine(t, ls, "PersistentVolumeClaim/win-db-01-log")
	if !strings.Contains(l, "event Warning ProvisioningFailed") {
		t.Errorf("the failing PVC must carry its provisioning event; got: %s", l)
	}
	if !strings.Contains(l, "DeadlineExceeded") {
		t.Errorf("the event message is the diagnosis and must be shown; got: %s", l)
	}
}

// A healthy report stays readable: events are not pasted onto everything unless
// asked for.
func TestEventsAreNotAttachedToHealthyObjectsByDefault(t *testing.T) {
	r := dumpResolver(t, "../../testdata/prodshape", "devc-org200", Options{})
	ls := lines(r.VM(context.Background(), "urt-f12", VMOptions{}))

	// The healthy bound PVC has no event note, though events exist in the namespace.
	l := findLine(t, ls, "PersistentVolumeClaim/restore-6f4117e6")
	if strings.Contains(l, "event ") {
		t.Errorf("healthy objects should not carry events by default; got: %s", l)
	}

	// With --events they appear everywhere there is one.
	all := dumpResolver(t, "../../testdata/prodshape", "devc-org200", Options{AllEvents: true})
	ls2 := lines(all.VM(context.Background(), "urt-f12", VMOptions{}))
	l2 := findLine(t, ls2, "PersistentVolumeClaim/restore-0bd08e71")
	if !strings.Contains(l2, "event Normal WaitForFirstConsumer") {
		t.Errorf("--events should surface Normal events too; got: %s", l2)
	}
}

func TestNoEventsFlagSuppressesCollection(t *testing.T) {
	r := dumpResolver(t, "../../testdata/fixture", "virt-prod", Options{NoEvents: true})
	ls := lines(r.Disks(context.Background(), "win-db-01"))
	for _, l := range ls {
		if strings.Contains(l, "event ") {
			t.Errorf("--no-events must suppress all events; got: %s", l)
		}
	}
}

// The tool must state the binding mode it read, and explain Pending accordingly,
// rather than offering both possibilities.
func TestPendingExplanationUsesRealBindingMode(t *testing.T) {
	r := dumpResolver(t, "../../testdata/fixture", "virt-prod", Options{})
	rep := r.Disks(context.Background(), "win-db-01")
	ls := lines(rep)

	l := findLine(t, ls, "PersistentVolumeClaim/win-db-01-log")
	if !strings.Contains(l, "bindingMode=Immediate") {
		t.Errorf("the storage class facts must be shown; got: %s", l)
	}
	var hint string
	for _, p := range rep.Problems {
		if strings.Contains(p.Message, `PVC phase is "Pending"`) {
			hint = p.Hint
		}
	}
	if !strings.Contains(hint, "Immediate") || !strings.Contains(hint, "provisioning itself is failing") {
		t.Errorf("hint must follow from the real binding mode; got: %q", hint)
	}
	if strings.Contains(hint, "WaitForFirstConsumer storage this is expected") {
		t.Errorf("must not offer the WFFC explanation when the class is Immediate: %q", hint)
	}
}

// "Why was this disk not snapshotted" is answered, not guessed at.
func TestUnsnapshottedDiskCauseIsVerified(t *testing.T) {
	r := dumpResolver(t, "../../testdata/fixture", "virt-prod", Options{})
	ls := lines(r.Snapshot(context.Background(), "win-db-01", "snap-2026-07-20"))

	l := findLine(t, ls, "volumeBackup[logdisk]")
	if !strings.Contains(l, "CONFIRMED cause") || !strings.Contains(l, "nfs.csi.k8s.io") {
		t.Errorf("the cause must be confirmed against the actual snapshot classes; got: %s", l)
	}
}

// A snapshottable class is reported as such, so it can be ruled out.
func TestSnapshotSupportIsReportedWhenPresent(t *testing.T) {
	r := dumpResolver(t, "../../testdata/prodshape", "devc-org200", Options{})
	ls := lines(r.VM(context.Background(), "urt-f12", VMOptions{}))
	l := findLine(t, ls, "PersistentVolumeClaim/restore-6f4117e6")
	if !strings.Contains(l, "snapshottable via VolumeSnapshotClass px-csi-snapclass (default)") {
		t.Errorf("expected snapshot support to be confirmed; got: %s", l)
	}
}

// The live view: what the VM actually has attached, and on which node.
func TestRuntimeViewReportsAttachedDisksAndNode(t *testing.T) {
	r := dumpResolver(t, "../../testdata/prodshape", "devc-org200", Options{})
	ls := lines(r.VM(context.Background(), "urt-f12", VMOptions{}))

	vmi := findLine(t, ls, "runtime VirtualMachineInstance/urt-f12")
	if !strings.Contains(vmi, "node=worker-03.pos-cloud.lan") {
		t.Errorf("the node holding the VM must be shown; got: %s", vmi)
	}
	if !strings.Contains(vmi, "LiveMigratable=False") {
		t.Errorf("expected the migratability condition; got: %s", vmi)
	}
	att := findLine(t, ls, "attached[rootdisk]")
	if !strings.Contains(att, "target=sda") || !strings.Contains(att, "phase=Ready") {
		t.Errorf("expected the live per-disk state; got: %s", att)
	}
	findLine(t, ls, "virt-launcher Pod/virt-launcher-urt-f12-x9k2p")
}

func TestRuntimeCanBeSkipped(t *testing.T) {
	r := dumpResolver(t, "../../testdata/prodshape", "devc-org200", Options{})
	ls := lines(r.VM(context.Background(), "urt-f12", VMOptions{SkipRuntime: true}))
	for _, l := range ls {
		if strings.Contains(l, "runtime VirtualMachineInstance") {
			t.Errorf("--no-runtime must omit the live view; got: %s", l)
		}
	}
}

// Where the volume is attached, and who else holds the PVC.
func TestAttachmentAndConsumersAreReported(t *testing.T) {
	r := dumpResolver(t, "../../testdata/prodshape", "devc-org200", Options{})
	ls := lines(r.VM(context.Background(), "urt-f12", VMOptions{}))

	va := findLine(t, ls, "VolumeAttachment/csi-attach-urt-f12-rootdisk")
	if !strings.Contains(va, "node=worker-03.pos-cloud.lan") || !strings.Contains(va, "attached=true") {
		t.Errorf("expected node and attach state; got: %s", va)
	}
	pvc := findLine(t, ls, "PersistentVolumeClaim/restore-6f4117e6")
	if !strings.Contains(pvc, "mounted by: virt-launcher-urt-f12-x9k2p") {
		t.Errorf("expected the consuming pod; got: %s", pvc)
	}
	// The stray PVC has no consumer, and saying so is the point.
	stray := findLine(t, ls, "PersistentVolumeClaim/restore-0bd08e71")
	if !strings.Contains(stray, "no pod currently mounts this PVC") {
		t.Errorf("expected an explicit no-consumer note; got: %s", stray)
	}
}

// The timeline establishes order, is deduplicated, and carries no zero times.
func TestTimelineIsOrderedAndClean(t *testing.T) {
	r := dumpResolver(t, "../../testdata/prodshape", "devc-org200", Options{Timeline: true})
	rep := r.VM(context.Background(), "urt-f12", VMOptions{})

	if len(rep.Timeline) == 0 {
		t.Fatal("expected timeline entries")
	}
	seen := map[string]bool{}
	for _, m := range rep.Timeline {
		if strings.HasPrefix(m.When, "0001-01-01") {
			t.Errorf("zero timestamp leaked into the timeline: %+v", m)
		}
		key := m.When + "|" + m.What
		if seen[key] {
			t.Errorf("duplicate timeline entry: %+v", m)
		}
		seen[key] = true
	}

	// The failed restore must appear, dated when it failed.
	var failed bool
	for _, m := range rep.Timeline {
		if strings.Contains(m.What, "FAILED") {
			failed = true
			if m.State != tree.StateError {
				t.Errorf("a failure moment should carry error state: %+v", m)
			}
		}
	}
	if !failed {
		t.Errorf("expected the failed restore in the timeline: %+v", rep.Timeline)
	}
}

func TestTimelineOffByDefault(t *testing.T) {
	r := dumpResolver(t, "../../testdata/prodshape", "devc-org200", Options{})
	rep := r.VM(context.Background(), "urt-f12", VMOptions{})
	if len(rep.Timeline) != 0 {
		t.Errorf("timeline must be opt-in, got %d entries", len(rep.Timeline))
	}
}

func TestParseQuantityBytes(t *testing.T) {
	cases := map[string]int64{
		"20Gi":  21474836480,
		"1.5Gi": 1610612736,
		"512":   512,
		"":      0,
		"weird": 0,
	}
	for in, want := range cases {
		if got := parseQuantityBytes(in); got != want {
			t.Errorf("parseQuantityBytes(%q) = %d, want %d", in, got, want)
		}
	}
}

// A snapshot bigger than the PVC it restores into cannot bind; that must be
// caught rather than surfacing later as an opaque CSI error.
func TestCapacityMismatchIsCaught(t *testing.T) {
	r := dumpResolver(t, "../../testdata/prodshape", "devc-org200", Options{})
	r.beginReport()
	node := tree.New("test")
	r.checkCapacityConsistency("20Gi", "", "100Gi", "PersistentVolumeClaim/x", node)

	if !node.Unhealthy() {
		t.Errorf("an undersized target must be flagged")
	}
	var found bool
	for _, p := range r.rep.Problems {
		if strings.Contains(p.Message, "snapshot is 100Gi but the target PVC only requests 20Gi") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a capacity problem; got %+v", r.rep.Problems)
	}

	// Equal sizes must stay silent.
	r.beginReport()
	quiet := tree.New("test")
	r.checkCapacityConsistency("20Gi", "20Gi", "20Gi", "x", quiet)
	if quiet.Unhealthy() || len(r.rep.Problems) != 0 {
		t.Errorf("matching sizes must not be flagged: %+v", r.rep.Problems)
	}
}

// The urt-f03 shape: the restore is stalled but the target VM is already Stopped,
// so advising "stop the target VM" would send the reader the wrong way.
func TestStalledRestoreHintRespectsStoppedTarget(t *testing.T) {
	r := dumpResolver(t, "../../testdata/provisionfail", "devc-org200", Options{})
	rep := r.VM(context.Background(), "urt-f03", VMOptions{})

	var hint string
	for _, p := range rep.Problems {
		if strings.Contains(p.Message, "restore has not completed") {
			hint = p.Hint
		}
	}
	if hint == "" {
		t.Fatalf("expected a stalled-restore problem; got %+v", rep.Problems)
	}
	if strings.Contains(hint, "stop it") || strings.Contains(hint, "will not restore into a running VM") {
		t.Errorf("must not tell the reader to stop an already-stopped VM: %q", hint)
	}
	if !strings.Contains(hint, "already Stopped") || !strings.Contains(hint, "NOT the blocker") {
		t.Errorf("hint should redirect to the real cause; got: %q", hint)
	}
}

// The driver's own error is the diagnosis and must reach the problem list, not
// sit only as a note deep in the tree.
func TestProvisioningFailureIsPromotedToAProblem(t *testing.T) {
	r := dumpResolver(t, "../../testdata/provisionfail", "devc-org200", Options{})
	rep := r.VM(context.Background(), "urt-f03", VMOptions{})

	var found bool
	for _, p := range rep.Problems {
		if strings.Contains(p.Message, "ProvisioningFailed") {
			found = true
			if !strings.Contains(p.Message, "No such volume or snapshot") {
				t.Errorf("the driver's message is the diagnosis; got: %s", p.Message)
			}
			if !strings.Contains(p.Message, "17 times") {
				t.Errorf("repeat count establishes this is persistent; got: %s", p.Message)
			}
			if !strings.Contains(p.Hint, "does not exist") || !strings.Contains(p.Hint, "readyToUse") {
				t.Errorf("hint should call out the stale readiness flag; got: %s", p.Hint)
			}
		}
	}
	if !found {
		t.Errorf("expected the provisioning failure as a problem; got %+v", rep.Problems)
	}
}

// Only the newest Warning is promoted; Normal events never are.
func TestOnlyNewestWarningIsPromoted(t *testing.T) {
	r := dumpResolver(t, "../../testdata/provisionfail", "devc-org200", Options{})
	rep := r.VM(context.Background(), "urt-f03", VMOptions{})

	var promoted int
	for _, p := range rep.Problems {
		if strings.Contains(p.Where, "restore-58252881-rootdisk") && strings.Contains(p.Message, "reports") {
			promoted++
		}
		if strings.Contains(p.Message, "ExternalProvisioning") {
			t.Errorf("Normal events must not be promoted: %+v", p)
		}
	}
	if promoted != 1 {
		t.Errorf("want exactly 1 promoted event problem, got %d: %+v", promoted, rep.Problems)
	}
}

// A snapshot whose array-side parent volume is gone is unrestorable, however
// confidently Kubernetes reports readyToUse=true.
func TestSnapshotWithMissingParentVolumeIsFlagged(t *testing.T) {
	r := dumpResolver(t, "../../testdata/provisionfail", "devc-org200", Options{})
	rep := r.VM(context.Background(), "urt-f03", VMOptions{})
	ls := lines(rep)

	l := findLine(t, ls, "PureSnapshot/c80fce94")
	if !strings.Contains(l, "parent volume px_fcb5d0f0-pvc-80eb6559-b5f0-4913-9b07-19bb22cb289c NOT found") {
		t.Errorf("expected the missing-parent note; got: %s", l)
	}
	var found bool
	for _, p := range rep.Problems {
		if strings.Contains(p.Message, "no longer exists on the array") {
			found = true
			if !strings.Contains(p.Hint, "unrestorable") {
				t.Errorf("hint should state the consequence; got: %s", p.Hint)
			}
		}
	}
	if !found {
		t.Errorf("expected a missing-parent-volume problem; got %+v", rep.Problems)
	}
}

// The check must stay quiet when the parent volume is present.
func TestSnapshotWithPresentParentVolumeIsQuiet(t *testing.T) {
	r := dumpResolver(t, "../../testdata/fixture", "virt-prod", Options{})
	rep := r.Snapshot(context.Background(), "win-db-01", "snap-2026-07-20")
	for _, p := range rep.Problems {
		if strings.Contains(p.Message, "no longer exists on the array") {
			t.Errorf("parent volume is present in this fixture, must not be flagged: %+v", p)
		}
	}
	ls := lines(rep)
	l := findLine(t, ls, "PureSnapshot/puresnapshot-win-db-01-root-snap-8f3a")
	if !strings.Contains(l, "still exists on the array") {
		t.Errorf("expected confirmation the parent is present; got: %s", l)
	}
}

// One fault reached from several branches must be reported once.
func TestProblemsAreDeduplicated(t *testing.T) {
	r := dumpResolver(t, "../../testdata/provisionfail", "devc-org200", Options{})
	rep := r.VM(context.Background(), "urt-f03", VMOptions{})

	seen := map[string]int{}
	for _, p := range rep.Problems {
		seen[p.Where+"|"+p.Message]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("problem reported %d times: %s", n, k)
		}
	}
}

// Portworx snapshot names come in two forms. When the name carries no parent
// volume the check cannot judge parent-death, and it must say so: silence would
// read as "checked and fine".
func TestParentVolumeCheckSaysWhenItCannotJudge(t *testing.T) {
	r := dumpResolver(t, "../../testdata/inplacerestore", "playground", Options{})
	ls := lines(r.VM(context.Background(), "vm-snapshot-test", VMOptions{}))

	l := findLine(t, ls, "PureSnapshot/5145aa09")
	if !strings.Contains(l, "cannot infer a parent volume") {
		t.Errorf("must state that the name carries no parent; got: %s", l)
	}
	if !strings.Contains(l, "origin volume lookup") {
		t.Errorf("should redirect to the check that does apply; got: %s", l)
	}
	// And it must not claim the parent is missing, which it cannot know here.
	for _, p := range r.rep.Problems {
		if strings.Contains(p.Message, "no longer exists on the array") {
			t.Errorf("must not assert a missing parent from an unparseable name: %+v", p)
		}
	}
}

// A CDI populator is indirection, not a different origin: VolumeCloneSource wraps
// the very PVC the DataVolume declares, so the "sources disagree" note must not
// fire on the wrapper's name.
func TestPopulatorIsResolvedAndNotReportedAsADisagreement(t *testing.T) {
	r := dumpResolver(t, "../../testdata/populator", "playground", Options{})
	ls := lines(r.VM(context.Background(), "vm-snapshot-test", VMOptions{}))

	l := findLine(t, ls, "spec.dataSourceRef VolumeCloneSource/volume-clone-source-923d03bf")
	if !strings.Contains(l, "populator wrapping PersistentVolumeClaim/fedora-1217dcc8c58d") {
		t.Errorf("the populator must resolve to the PVC it wraps; got: %s", l)
	}
	if strings.Contains(l, "separately declares its source as") {
		t.Errorf("sources agree once the populator is resolved, so no disagreement note: %s", l)
	}
}

// The disagreement note must still fire when the sources genuinely differ, which
// is the case after a restore repopulates the volume from a snapshot.
func TestGenuineSourceDisagreementStillReported(t *testing.T) {
	r := dumpResolver(t, "../../testdata/inplacerestore", "playground", Options{})
	ls := lines(r.VM(context.Background(), "vm-snapshot-test", VMOptions{}))
	l := findLine(t, ls, "spec.dataSourceRef VolumeSnapshot/vmsnapshot-b7fee618")
	if !strings.Contains(l, "separately declares its source as") {
		t.Errorf("a real disagreement must still be reported; got: %s", l)
	}
}

// A name is not an identity: an InPlace restore recreates the PVC under the same
// name, so a PVC newer than the snapshot is not the object that was captured.
func TestReusedPVCNameIsNotMistakenForTheCapturedDisk(t *testing.T) {
	after := dumpResolver(t, "../../testdata/inplacerestore", "playground", Options{})
	ls := lines(after.VM(context.Background(), "vm-snapshot-test", VMOptions{}))
	l := findLine(t, ls, "volumeBackup[rootdisk]")
	if !strings.Contains(l, "AFTER this snapshot was taken") {
		t.Errorf("a newer same-named PVC must be called out; got: %s", l)
	}
	if !strings.Contains(l, "only copy of the original disk") {
		t.Errorf("the consequence must be stated; got: %s", l)
	}
	if strings.Contains(l, "still exists (created") {
		t.Errorf("must not claim the captured PVC survives; got: %s", l)
	}

	// The pre-restore state has the genuine PVC and must read as intact.
	before := dumpResolver(t, "../../testdata/populator", "playground", Options{})
	bl := lines(before.VM(context.Background(), "vm-snapshot-test", VMOptions{}))
	b := findLine(t, bl, "volumeBackup[rootdisk]")
	if !strings.Contains(b, "still exists (created") {
		t.Errorf("the original PVC predates the snapshot and should read as intact; got: %s", b)
	}
	if strings.Contains(b, "AFTER this snapshot") {
		t.Errorf("false positive on a genuinely surviving PVC: %s", b)
	}
}

// The restore policy that explains the reused name must be visible.
func TestVolumeRestorePolicyIsShown(t *testing.T) {
	r := dumpResolver(t, "../../testdata/inplacerestore", "playground", Options{})
	ls := lines(r.VM(context.Background(), "vm-snapshot-test", VMOptions{}))
	l := findLine(t, ls, "VirtualMachineRestore/restore-vm-snapshot-test")
	if !strings.Contains(l, "volumeRestorePolicy=InPlace") {
		t.Errorf("InPlace is why the PVC name was reused and must be shown; got: %s", l)
	}
}
