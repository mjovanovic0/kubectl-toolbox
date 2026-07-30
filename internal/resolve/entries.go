package resolve

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mjovanovic0/kubectl-vmsnap/internal/source"
	"github.com/mjovanovic0/kubectl-vmsnap/internal/tree"
)

// SnapshotRow is one line of the list output.
type SnapshotRow struct {
	Name    string
	VM      string
	Phase   string
	Ready   string
	Created string
	Content string
	Volumes string
	Issues  string
}

// ListSnapshots lists VirtualMachineSnapshots, optionally filtered to one VM.
// vmName == "" lists every snapshot in the namespace.
func (r *Resolver) ListSnapshots(ctx context.Context, vmName string) ([]SnapshotRow, error) {
	snaps, err := r.src.List(ctx, "VirtualMachineSnapshot", "snapshot.kubevirt.io", r.opt.Namespace)
	if err != nil {
		return nil, err
	}
	rows := make([]SnapshotRow, 0, len(snaps))
	for _, s := range snaps {
		srcName := nstr(s.Object, "spec", "source", "name")
		if vmName != "" && srcName != vmName {
			continue
		}
		row := SnapshotRow{
			Name:    s.GetName(),
			VM:      srcName,
			Phase:   orDash(nstr(s.Object, "status", "phase")),
			Ready:   orDash(boolStr(s.Object, "status", "readyToUse")),
			Created: orDash(nstr(s.Object, "status", "creationTime")),
			Content: orDash(nstr(s.Object, "status", "virtualMachineSnapshotContentName")),
			Volumes: "-",
		}

		var issues []string
		if msg := errMessage(s.Object, "status", "error"); msg != "" {
			issues = append(issues, "error: "+truncate(msg, 60))
		}
		if exc := stringSlice(nslice(s.Object, "status", "snapshotVolumes", "excludedVolumes")); len(exc) > 0 {
			issues = append(issues, "excluded: "+strings.Join(exc, ","))
		}

		// Counting volumes requires the content object; its absence is itself the
		// most important thing to report here.
		contentName := nstr(s.Object, "status", "virtualMachineSnapshotContentName")
		if contentName != "" {
			content, cerr := r.src.Get(ctx, source.Ref{
				Kind: "VirtualMachineSnapshotContent", Namespace: s.GetNamespace(), Name: contentName,
			})
			if cerr != nil {
				issues = append(issues, "content MISSING")
			} else {
				backups := nslice(content.Object, "spec", "volumeBackups")
				noSnap := 0
				for _, raw := range backups {
					if nstr(asMap(raw), "volumeSnapshotName") == "" {
						noSnap++
					}
				}
				row.Volumes = fmt.Sprintf("%d", len(backups))
				if noSnap > 0 {
					issues = append(issues, fmt.Sprintf("%d disk(s) not snapshotted", noSnap))
				}
			}
		}
		row.Issues = strings.Join(issues, "; ")
		if row.Issues == "" {
			row.Issues = "-"
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Created != rows[j].Created {
			return rows[i].Created < rows[j].Created
		}
		return rows[i].Name < rows[j].Name
	})
	return rows, nil
}

// PVCChain resolves a PVC downward to the array, plus its provenance upward.
func (r *Resolver) PVCChain(ctx context.Context, name string) *tree.Report {
	r.beginReport()
	root := tree.New("namespace/" + r.opt.Namespace).SetState(tree.StateInfo)
	r.rep.Root = root
	r.describePVC(ctx, r.opt.Namespace, name, root, 0, pvcContext{})
	return r.finish(ctx)
}

// VolumeSnapshotChain resolves a bare VolumeSnapshot to the array, for cases
// where you have the CSI-level name but not the VM snapshot it belongs to.
func (r *Resolver) VolumeSnapshotChain(ctx context.Context, name string) *tree.Report {
	r.beginReport()
	vs, node := r.getOrNode(ctx,
		source.Ref{Kind: "VolumeSnapshot", Namespace: r.opt.Namespace, Name: name}, "VolumeSnapshot lookup")
	r.rep.Root = node
	if vs == nil {
		return r.finish(ctx)
	}
	r.describeVolumeSnapshot(ctx, vs, node, "")

	// Point back at the owning VirtualMachineSnapshotContent, if any.
	for _, c := range r.listKind(ctx, "VirtualMachineSnapshotContent", r.opt.Namespace) {
		for _, raw := range nslice(c.Object, "spec", "volumeBackups") {
			vb := asMap(raw)
			if nstr(vb, "volumeSnapshotName") != name {
				continue
			}
			n := node.Add(tree.New("⤴ VirtualMachineSnapshotContent/" + c.GetName()).SetState(tree.StateInfo))
			n.Attr("disk", nstr(vb, "volumeName"))
			n.Attr("snapshot", nstr(c.Object, "spec", "virtualMachineSnapshotName"))
			n.Attr("sourcePVC", nstr(vb, "persistentVolumeClaim", "metadata", "name"))
		}
	}
	return r.finish(ctx)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
