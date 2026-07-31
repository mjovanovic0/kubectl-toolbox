package resolve

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/mjovanovic0/kubectl-toolbox/internal/source"
	"github.com/mjovanovic0/kubectl-toolbox/internal/tree"
)

// VMOptions control the combined per-VM view.
type VMOptions struct {
	// SkipDisks / SkipSnapshots narrow the report to one half.
	SkipDisks     bool
	SkipSnapshots bool
	// Latest limits the report to the N most recent snapshots. 0 means all.
	Latest int
	// SkipRuntime omits the live VMI/virt-launcher view.
	SkipRuntime bool
}

// VM resolves everything about one VM in a single pass: its live disks down to
// the array, and every VirtualMachineSnapshot of it down to the array.
func (r *Resolver) VM(ctx context.Context, vmName string, vopt VMOptions) *tree.Report {
	ns := r.opt.Namespace
	r.beginReport()

	vm, vmNode := r.getOrNode(ctx, source.Ref{Kind: "VirtualMachine", Namespace: ns, Name: vmName}, "VirtualMachine lookup")
	r.rep.Root = vmNode
	if vm != nil {
		r.describeVMNode(vm, vmNode)
	} else {
		// Snapshots outlive their VM, so keep going -- that is exactly the case
		// where you need this tool.
		vmNode.Note("VM not found; reporting any snapshots that outlived it")
	}

	if vm != nil && !vopt.SkipRuntime {
		r.addRuntime(ctx, vm, vmNode)
	}

	if !vopt.SkipDisks && vm != nil {
		disks := vmNode.Add(tree.New("disks").SetState(tree.StateInfo))
		r.addDisks(ctx, vm, disks)
		disks.Label = fmt.Sprintf("disks (%d)", len(disks.Children))
	}

	if !vopt.SkipSnapshots {
		r.addVMSnapshots(ctx, ns, vmName, vmNode, vopt.Latest)
	}
	return r.finish(ctx)
}

// addVMSnapshots resolves every snapshot whose source is this VM.
func (r *Resolver) addVMSnapshots(ctx context.Context, ns, vmName string, vmNode *tree.Node, latest int) {
	snaps, err := r.src.List(ctx, "VirtualMachineSnapshot", "snapshot.kubevirt.io", ns)
	if err != nil {
		vmNode.Add(tree.New("snapshots").Escalate(tree.StateUnknown)).
			Note("could not list VirtualMachineSnapshots: " + err.Error())
		return
	}

	mine := make([]*unstructured.Unstructured, 0, len(snaps))
	for _, s := range snaps {
		if nstr(s.Object, "spec", "source", "name") == vmName {
			mine = append(mine, s)
		}
	}
	// Newest last, matching the list command, so the tree reads chronologically.
	sort.Slice(mine, func(i, j int) bool {
		ti := nstr(mine[i].Object, "status", "creationTime")
		tj := nstr(mine[j].Object, "status", "creationTime")
		if ti != tj {
			return ti < tj
		}
		return mine[i].GetName() < mine[j].GetName()
	})

	total := len(mine)
	dropped := 0
	if latest > 0 && total > latest {
		dropped = total - latest
		mine = mine[dropped:]
	}

	node := vmNode.Add(tree.New(fmt.Sprintf("snapshots (%d)", total)).SetState(tree.StateInfo))
	if total == 0 {
		node.Note("no VirtualMachineSnapshot has this VM as its spec.source")
		return
	}
	if dropped > 0 {
		node.Note(fmt.Sprintf("showing the %d most recent; %d older snapshot(s) not resolved (raise --latest to include them)", latest, dropped))
	}

	for _, s := range mine {
		// The object is already in hand from the List; no need to re-Get it.
		snapNode := node.Add(tree.New("VirtualMachineSnapshot/" + s.GetName()))
		r.describeVMSnapshot(ctx, s, snapNode, vmName)
	}
}
