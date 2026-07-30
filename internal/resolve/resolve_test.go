package resolve

import (
	"context"
	"strings"
	"testing"

	"github.com/mjovanovic0/kubectl-vmsnap/internal/source"
	"github.com/mjovanovic0/kubectl-vmsnap/internal/tree"
)

const fixture = "../../testdata/fixture"

func newFixtureResolver(t *testing.T) *Resolver {
	t.Helper()
	d, err := source.NewDump(fixture)
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	return New(d, Options{Namespace: "virt-prod"})
}

// flatten renders the tree into "label|attrs|notes|state" lines for assertions.
func flatten(n *tree.Node, out *[]string) {
	if n == nil {
		return
	}
	var sb strings.Builder
	sb.WriteString(n.Label)
	for _, a := range n.Attrs {
		sb.WriteString(" " + a.Key + "=" + a.Val)
	}
	for _, note := range n.Notes {
		sb.WriteString(" #" + note)
	}
	sb.WriteString(" @" + stateName(n.State))
	*out = append(*out, sb.String())
	for _, c := range n.Children {
		flatten(c, out)
	}
}

func stateName(s tree.State) string {
	switch s {
	case tree.StateOK:
		return "ok"
	case tree.StateInfo:
		return "info"
	case tree.StateWarn:
		return "warn"
	case tree.StateMissing:
		return "missing"
	case tree.StateError:
		return "error"
	}
	return "unknown"
}

func lines(rep *tree.Report) []string {
	var out []string
	flatten(rep.Root, &out)
	return out
}

func findLine(t *testing.T, ls []string, substr string) string {
	t.Helper()
	for _, l := range ls {
		if strings.Contains(l, substr) {
			return l
		}
	}
	t.Fatalf("no line containing %q in:\n%s", substr, strings.Join(ls, "\n"))
	return ""
}

func TestSnapshotResolvesHealthyChainToArray(t *testing.T) {
	r := newFixtureResolver(t)
	rep := r.Snapshot(context.Background(), "win-db-01", "snap-2026-07-20")
	ls := lines(rep)

	// The healthy disk must resolve all the way to the array object.
	l := findLine(t, ls, "PureSnapshot/puresnapshot-win-db-01-root-snap-8f3a")
	for _, want := range []string{
		"matchedOn=snapshotName",
		"snapshotID=3921",
		"podName=vg-prod",
		"storageBackendType=FlashArray",
		"targets=10.20.30.10,10.20.30.11",
	} {
		if !strings.Contains(l, want) {
			t.Errorf("array node missing %q; got: %s", want, l)
		}
	}

	// The CSI handle must be surfaced verbatim -- it is what you search on the array.
	vsc := findLine(t, ls, "VolumeSnapshotContent/snapcontent-1c2d")
	if !strings.Contains(vsc, "snapshotHandle=pure-fa1::win-db-01-root.snap-8f3a") {
		t.Errorf("expected snapshotHandle attr; got: %s", vsc)
	}
	// restoreSize arrives as int64 nanobytes-style raw bytes here and must be humanised.
	if !strings.Contains(vsc, "restoreSize=20Gi") {
		t.Errorf("expected int64 restoreSize rendered as 20Gi; got: %s", vsc)
	}

	// The captured source PVC spec must be shown even though we also fetch the PVC.
	vb := findLine(t, ls, "volumeBackup[rootdisk]")
	if !strings.Contains(vb, "pvc=win-db-01-root") || !strings.Contains(vb, "size=20Gi") || !strings.Contains(vb, "sc=pure-block") {
		t.Errorf("volumeBackup should carry captured PVC spec; got: %s", vb)
	}
}

func TestSnapshotFlagsDanglingVolumeSnapshot(t *testing.T) {
	r := newFixtureResolver(t)
	rep := r.Snapshot(context.Background(), "win-db-01", "snap-2026-07-20")
	ls := lines(rep)

	l := findLine(t, ls, "VolumeSnapshot/vmsnapshot-8f3a-vol-datadisk")
	if !strings.Contains(l, "@missing") {
		t.Errorf("dangling VolumeSnapshot should be missing; got: %s", l)
	}
	// The content-side error is the only surviving evidence of why it failed.
	if !strings.Contains(l, "DeadlineExceeded") {
		t.Errorf("expected content-recorded error to be surfaced; got: %s", l)
	}
	// The captured spec of the already-deleted PVC must still be reported.
	vb := findLine(t, ls, "volumeBackup[datadisk]")
	if !strings.Contains(vb, "pvc=win-db-01-data") || !strings.Contains(vb, "size=100Gi") {
		t.Errorf("expected captured spec for deleted PVC; got: %s", vb)
	}
	if !strings.Contains(vb, "no longer exists") {
		t.Errorf("expected note that source PVC is gone; got: %s", vb)
	}
}

func TestSnapshotFlagsNeverSnapshottedDisk(t *testing.T) {
	r := newFixtureResolver(t)
	rep := r.Snapshot(context.Background(), "win-db-01", "snap-2026-07-20")
	ls := lines(rep)

	vb := findLine(t, ls, "volumeBackup[logdisk]")
	if !strings.Contains(vb, "@missing") || !strings.Contains(vb, "never snapshotted") {
		t.Errorf("logdisk has no volumeSnapshotName and must be flagged; got: %s", vb)
	}
	// excludedVolumes explains why, and must appear on the snapshot node.
	snap := findLine(t, ls, "VirtualMachineSnapshot/snap-2026-07-20")
	if !strings.Contains(snap, "EXCLUDED: logdisk") {
		t.Errorf("expected excludedVolumes note; got: %s", snap)
	}

	if got := rep.WorstState(); got != tree.StateMissing {
		t.Errorf("WorstState = %s, want missing", stateName(got))
	}
	if len(rep.Problems) != 2 {
		t.Errorf("want 2 problems, got %d: %+v", len(rep.Problems), rep.Problems)
	}
}

func TestSnapshotDetectsWrongVM(t *testing.T) {
	r := newFixtureResolver(t)
	rep := r.Snapshot(context.Background(), "some-other-vm", "snap-2026-07-20")
	var found bool
	for _, p := range rep.Problems {
		if strings.Contains(p.Message, "not the VM") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a source-VM mismatch problem, got: %+v", rep.Problems)
	}
}

func TestDisksResolvesThroughDataVolumeToArray(t *testing.T) {
	r := newFixtureResolver(t)
	rep := r.Disks(context.Background(), "win-db-01")
	ls := lines(rep)

	pv := findLine(t, ls, "PureVolume/purevolume-win-db-01-root")
	for _, want := range []string{"serial=F4A29B31C7D84E0600011234", "volumeID=88213", "matchedOn=volumeName", "secure=true"} {
		if !strings.Contains(pv, want) {
			t.Errorf("PureVolume node missing %q; got: %s", want, pv)
		}
	}
	if l := findLine(t, ls, "PersistentVolume/pv-win-db-01-root"); !strings.Contains(l, "volumeHandle=pure-fa1::win-db-01-root") {
		t.Errorf("expected CSI volumeHandle; got: %s", l)
	}
}

func TestDisksWalksImportLineageUpward(t *testing.T) {
	r := newFixtureResolver(t)
	rep := r.Disks(context.Background(), "win-db-01")
	ls := lines(rep)

	// DataSource is the layer above DataVolume, DataImportCron the one above that.
	findLine(t, ls, "sourceRef DataSource/win2022")
	cron := findLine(t, ls, "DataImportCron/win2022-image-cron")
	if !strings.Contains(cron, "schedule=0 3 * * *") {
		t.Errorf("expected cron schedule; got: %s", cron)
	}
	if !strings.Contains(cron, "lastImportedPVC: win2022-abc123") {
		t.Errorf("expected lastImportedPVC note; got: %s", cron)
	}
}

// A bound PVC whose origin snapshot has rotated away is normal lineage, not a
// fault, and must not drive a non-zero exit.
func TestDisksDoesNotFailOnRotatedGoldenSnapshot(t *testing.T) {
	r := newFixtureResolver(t)
	rep := r.Disks(context.Background(), "win-db-01")
	ls := lines(rep)

	l := findLine(t, ls, "dataSourceRef VolumeSnapshot/win2022-golden-snap")
	if strings.Contains(l, "@missing") {
		t.Errorf("rotated origin snapshot of a Bound PVC must not be MISSING; got: %s", l)
	}
	if !strings.Contains(l, "expected once the PVC is populated") {
		t.Errorf("expected an explanatory note; got: %s", l)
	}
	for _, p := range rep.Problems {
		if strings.Contains(p.Message, "win2022-golden-snap") {
			t.Errorf("rotated origin snapshot must not be reported as a problem: %+v", p)
		}
	}
}

func TestDisksFlagsMissingPVCAndUnboundPVC(t *testing.T) {
	r := newFixtureResolver(t)
	rep := r.Disks(context.Background(), "win-db-01")
	ls := lines(rep)

	if l := findLine(t, ls, "PersistentVolumeClaim/win-db-01-data"); !strings.Contains(l, "@missing") {
		t.Errorf("absent PVC must be missing; got: %s", l)
	}
	if l := findLine(t, ls, "PersistentVolumeClaim/win-db-01-log"); !strings.Contains(l, "@warn") {
		t.Errorf("Pending PVC must warn; got: %s", l)
	}
	// A dataVolumeTemplate with no DataVolume is the classic missing-disk symptom.
	findLine(t, ls, "dataVolumeTemplate[win-db-01-data]")
}

func TestVolumeSnapshotChainFindsOwningContent(t *testing.T) {
	r := newFixtureResolver(t)
	rep := r.VolumeSnapshotChain(context.Background(), "vmsnapshot-8f3a-vol-rootdisk")
	ls := lines(rep)

	owner := findLine(t, ls, "VirtualMachineSnapshotContent/vmsnapshot-content-8f3a")
	if !strings.Contains(owner, "disk=rootdisk") || !strings.Contains(owner, "snapshot=snap-2026-07-20") {
		t.Errorf("expected upward link to the VM snapshot; got: %s", owner)
	}
	findLine(t, ls, "PureSnapshot/")
}

func TestListSnapshotsFlagsIssues(t *testing.T) {
	r := newFixtureResolver(t)
	rows, err := r.ListSnapshots(context.Background(), "win-db-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	row := rows[0]
	if row.Volumes != "3" {
		t.Errorf("want 3 volumes, got %q", row.Volumes)
	}
	if !strings.Contains(row.Issues, "1 disk(s) not snapshotted") {
		t.Errorf("want not-snapshotted issue, got %q", row.Issues)
	}
	if !strings.Contains(row.Issues, "excluded: logdisk") {
		t.Errorf("want excluded note, got %q", row.Issues)
	}
}

// Filtering by a VM with no snapshots must return nothing rather than everything.
func TestListSnapshotsFiltersByVM(t *testing.T) {
	r := newFixtureResolver(t)
	rows, err := r.ListSnapshots(context.Background(), "nonexistent-vm")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("want 0 rows, got %d", len(rows))
	}
}

func TestHandleCandidates(t *testing.T) {
	got := handleCandidates("pure-fa1::win-db-01-root.snap-8f3a")
	want := "win-db-01-root.snap-8f3a"
	var found bool
	for _, c := range got {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %q among candidates %v", want, got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		21474836480: "20Gi",
		1073741824:  "1Gi",
		1536:        "1.5Ki",
		512:         "512",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
