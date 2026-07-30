package resolve

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/mjovanovic0/kubectl-vmsnap/internal/source"
	"github.com/mjovanovic0/kubectl-vmsnap/internal/tree"
)

// Snapshot resolves the full chain
//
//	VirtualMachine
//	  -> VirtualMachineSnapshot           (status.virtualMachineSnapshotContentName)
//	  -> VirtualMachineSnapshotContent    (spec.volumeBackups[], status.volumeSnapshotStatus[])
//	  -> VolumeSnapshot                   (status.boundVolumeSnapshotContentName)
//	  -> VolumeSnapshotContent            (status.snapshotHandle)
//	  -> vendor array CR                  (PureSnapshot)
func (r *Resolver) Snapshot(ctx context.Context, vmName, snapName string) *tree.Report {
	ns := r.opt.Namespace
	r.beginReport()

	snapObj, snapNode := r.getOrNode(ctx,
		source.Ref{Kind: "VirtualMachineSnapshot", Namespace: ns, Name: snapName},
		"VirtualMachineSnapshot lookup")

	// The VM is the source of the snapshot, and may legitimately be gone.
	root := r.vmRootNode(ctx, ns, vmName, snapObj)
	r.rep.Root = root
	root.Add(snapNode)

	if snapObj == nil {
		return r.finish(ctx)
	}
	r.describeVMSnapshot(ctx, snapObj, snapNode, vmName)
	return r.finish(ctx)
}

// vmRootNode builds the root node for the VM, tolerating its absence.
func (r *Resolver) vmRootNode(ctx context.Context, ns, vmName string, snapObj *unstructured.Unstructured) *tree.Node {
	vm, err := r.src.Get(ctx, source.Ref{Kind: "VirtualMachine", Group: "kubevirt.io", Namespace: ns, Name: vmName})
	if err != nil {
		n := tree.New("VirtualMachine/" + vmName).Escalate(tree.StateWarn).
			Note("VM not found; resolving the snapshot chain anyway")
		r.rep.Problem(tree.StateWarn, "VirtualMachine/"+vmName,
			"the source VM does not exist",
			"a snapshot outliving its VM is normal after a VM delete; the captured spec lives in VirtualMachineSnapshotContent.spec.source.virtualMachine")
		return n
	}
	n := tree.New("VirtualMachine/" + vmName)
	n.Attr("runStrategy", nstr(vm.Object, "spec", "runStrategy"))
	if b, found := nbool(vm.Object, "spec", "running"); found {
		n.Attr("running", fmt.Sprintf("%t", b))
	}
	n.Attr("printableStatus", nstr(vm.Object, "status", "printableStatus"))
	n.Attr("uid", truncate(string(vm.GetUID()), 8))
	return n
}

func (r *Resolver) describeVMSnapshot(ctx context.Context, snap *unstructured.Unstructured, node *tree.Node, vmName string) {
	ns := snap.GetNamespace()
	where := "VirtualMachineSnapshot/" + snap.GetName()

	phase := nstr(snap.Object, "status", "phase")
	ready := boolStr(snap.Object, "status", "readyToUse")
	node.Attr("phase", phase)
	node.Attr("ready", ready)
	node.Attr("created", nstr(snap.Object, "status", "creationTime"))
	node.Attr("deletionPolicy", nstr(snap.Object, "spec", "deletionPolicy"))

	if phase != "" && phase != "Succeeded" {
		node.Escalate(tree.StateWarn)
		r.rep.Problem(tree.StateWarn, where,
			fmt.Sprintf("snapshot phase is %q, not Succeeded", phase),
			"check status.error and status.conditions on the snapshot and its content")
	}
	if ready == "false" {
		node.Escalate(tree.StateWarn)
		r.rep.Problem(tree.StateWarn, where, "snapshot is not readyToUse",
			"an unready snapshot cannot be restored from")
	}
	if msg := errMessage(snap.Object, "status", "error"); msg != "" {
		node.Escalate(tree.StateError)
		node.Note("error: " + msg)
		r.rep.Problem(tree.StateError, where, "snapshot reports an error: "+msg, "")
	}

	// spec.source is a TypedLocalObjectReference to the VM.
	srcKind := nstr(snap.Object, "spec", "source", "kind")
	srcName := nstr(snap.Object, "spec", "source", "name")
	if srcName != "" && srcName != vmName {
		node.Escalate(tree.StateWarn)
		r.rep.Problem(tree.StateWarn, where,
			fmt.Sprintf("snapshot source is %s/%s, not the VM %q you asked about", srcKind, srcName, vmName),
			"you are looking at a snapshot of a different VM")
	}

	// Indications tell you whether the snapshot was online, had guest agent
	// participation, etc. -- decisive when a restore produces a dirty disk.
	if ind := indications(snap.Object); ind != "" {
		node.Attr("indications", ind)
	}
	if v := volumeLists(snap.Object); v != "" {
		node.Note(v)
	}
	r.rep.At(nstr(snap.Object, "status", "creationTime"),
		"VirtualMachineSnapshot "+snap.GetName()+" taken (phase "+orDash(phase)+")", tree.StateInfo)
	if snap.GetDeletionTimestamp() != nil {
		node.Escalate(tree.StateWarn)
		node.Note("terminating since " + snap.GetDeletionTimestamp().String())
		if f := snap.GetFinalizers(); len(f) > 0 {
			node.Note("finalizers blocking deletion: " + strings.Join(f, ", "))
		}
	}

	if r.opt.ShowOwners {
		r.addProducers(ctx, snap, node)
	}
	r.addRestores(ctx, ns, snap.GetName(), node)

	contentName := nstr(snap.Object, "status", "virtualMachineSnapshotContentName")
	contentObj, contentNode := r.getOrNode(ctx,
		source.Ref{Kind: "VirtualMachineSnapshotContent", Namespace: ns, Name: contentName},
		where+" -> status.virtualMachineSnapshotContentName")
	node.Add(contentNode)
	if contentObj == nil {
		return
	}
	r.describeVMSnapshotContent(ctx, contentObj, contentNode, snap)
}

func (r *Resolver) describeVMSnapshotContent(ctx context.Context, content *unstructured.Unstructured, node *tree.Node, snap *unstructured.Unstructured) {
	ns := content.GetNamespace()
	where := "VirtualMachineSnapshotContent/" + content.GetName()

	node.Attr("ready", boolStr(content.Object, "status", "readyToUse"))
	node.Attr("created", nstr(content.Object, "status", "creationTime"))
	if msg := errMessage(content.Object, "status", "error"); msg != "" {
		node.Escalate(tree.StateError)
		node.Note("error: " + msg)
		r.rep.Problem(tree.StateError, where, "content reports an error: "+msg, "")
	}

	// Verify the back-reference. A content whose spec points at a different
	// snapshot name is a real, if rare, corruption.
	if back := nstr(content.Object, "spec", "virtualMachineSnapshotName"); back != "" && back != snap.GetName() {
		node.Escalate(tree.StateWarn)
		r.rep.Problem(tree.StateWarn, where,
			fmt.Sprintf("content spec.virtualMachineSnapshotName is %q but it is referenced by snapshot %q", back, snap.GetName()),
			"the content and snapshot disagree about their pairing")
	}

	// status.volumeSnapshotStatus[] is per-VolumeSnapshot state, keyed by name.
	vsStatus := map[string]map[string]interface{}{}
	for _, raw := range nslice(content.Object, "status", "volumeSnapshotStatus") {
		m := asMap(raw)
		if n := nstr(m, "volumeSnapshotName"); n != "" {
			vsStatus[n] = m
		}
	}

	// spec.volumeBackups[] is the authoritative per-disk record: it carries the
	// VM disk name, the VolumeSnapshot name, and the captured source PVC spec.
	backups := nslice(content.Object, "spec", "volumeBackups")
	if len(backups) == 0 {
		node.Escalate(tree.StateWarn)
		node.Note("spec.volumeBackups is empty: no disks were captured")
		r.rep.Problem(tree.StateWarn, where, "no volumeBackups recorded",
			"the VM had no snapshottable volumes, or the snapshot failed before capturing them; PVCs whose storage class lacks a VolumeSnapshotClass are silently skipped")
	}

	seen := map[string]bool{}
	for _, raw := range backups {
		vb := asMap(raw)
		r.describeVolumeBackup(ctx, ns, vb, node, vsStatus, seen)
	}

	// Any status entry with no matching backup is an inconsistency worth showing.
	for name := range vsStatus {
		if !seen[name] {
			n := node.Add(tree.New("volumeSnapshotStatus[" + name + "]").Escalate(tree.StateWarn))
			n.Note("present in status.volumeSnapshotStatus but absent from spec.volumeBackups")
			r.rep.Problem(tree.StateWarn, where,
				fmt.Sprintf("status tracks VolumeSnapshot %q that no volumeBackup declares", name),
				"stale status entry, usually after a partially failed snapshot")
		}
	}
}

// describeVolumeBackup renders one spec.volumeBackups[] entry and dives into its
// VolumeSnapshot.
func (r *Resolver) describeVolumeBackup(
	ctx context.Context,
	ns string,
	vb map[string]interface{},
	parent *tree.Node,
	vsStatus map[string]map[string]interface{},
	seen map[string]bool,
) {
	diskName := nstr(vb, "volumeName")
	node := parent.Add(tree.New("volumeBackup[" + diskName + "]"))

	pvcName := nstr(vb, "persistentVolumeClaim", "metadata", "name")
	size := nqty(vb, "persistentVolumeClaim", "spec", "resources", "requests", "storage")
	sc := nstr(vb, "persistentVolumeClaim", "spec", "storageClassName")
	mode := nstr(vb, "persistentVolumeClaim", "spec", "volumeMode")
	node.Attr("pvc", pvcName)
	node.Attr("size", size)
	node.Attr("sc", sc)
	node.Attr("volumeMode", mode)

	// Is the original PVC still around? Decisive when debugging a failed restore.
	if pvcName != "" {
		if _, err := r.src.Get(ctx, source.Ref{Kind: "PersistentVolumeClaim", Namespace: ns, Name: pvcName}); err != nil {
			node.Note("source PVC " + pvcName + " no longer exists (captured spec above is the only record)")
			node.Note("this snapshot is now the only copy of that disk -- deleting it discards the array snapshot too")
		} else {
			node.Note("source PVC " + pvcName + " still exists")
		}
	}

	vsName := nstr(vb, "volumeSnapshotName")
	if vsName == "" {
		node.Escalate(tree.StateMissing)
		node.Note("volumeSnapshotName is unset: this disk was never snapshotted")
		// Do not guess at the cause: check whether the captured storage class
		// actually lacks a VolumeSnapshotClass, and say which it is.
		r.explainUnsnapshottedDisk(ctx, sc, node)
		r.rep.Problem(tree.StateMissing, fmt.Sprintf("volumeBackup[%s]", diskName),
			"no VolumeSnapshot was created for this disk",
			"the note on this node states whether the storage class lacks a VolumeSnapshotClass, which is the usual cause")
		return
	}
	seen[vsName] = true

	vsObj, vsNode := r.getOrNode(ctx,
		source.Ref{Kind: "VolumeSnapshot", Namespace: ns, Name: vsName},
		fmt.Sprintf("volumeBackup[%s].volumeSnapshotName", diskName))
	node.Add(vsNode)

	// Content-side status for this VolumeSnapshot, useful even when the
	// VolumeSnapshot object itself is gone.
	if st, ok := vsStatus[vsName]; ok {
		vsNode.Attr("content.ready", boolStr(st, "readyToUse"))
		if msg := errMessage(st, "error"); msg != "" {
			vsNode.Note("content-recorded error: " + msg)
		}
	} else {
		vsNode.Note("no matching entry in content status.volumeSnapshotStatus")
	}

	if vsObj == nil {
		return
	}
	r.describeVolumeSnapshot(ctx, vsObj, vsNode, pvcName)
}

func (r *Resolver) describeVolumeSnapshot(ctx context.Context, vs *unstructured.Unstructured, node *tree.Node, expectPVC string) {
	where := "VolumeSnapshot/" + vs.GetName()

	ready := boolStr(vs.Object, "status", "readyToUse")
	node.Attr("ready", ready)
	node.Attr("restoreSize", nqty(vs.Object, "status", "restoreSize"))
	node.Attr("class", nstr(vs.Object, "spec", "volumeSnapshotClassName"))
	node.Attr("created", nstr(vs.Object, "status", "creationTime"))
	r.rep.At(nstr(vs.Object, "status", "creationTime"),
		"VolumeSnapshot "+vs.GetName()+" created on the array", tree.StateInfo)

	if ready == "false" || ready == "" {
		node.Escalate(tree.StateWarn)
		r.rep.Problem(tree.StateWarn, where, "VolumeSnapshot is not readyToUse",
			"the CSI driver has not finished (or failed) creating the array-side snapshot")
	}
	if msg := errMessage(vs.Object, "status", "error"); msg != "" {
		node.Escalate(tree.StateError)
		node.Note("error: " + msg)
		r.rep.Problem(tree.StateError, where, "VolumeSnapshot error: "+msg, "")
	}

	// spec.source is either a live PVC (dynamic) or a pre-provisioned content.
	srcPVC := nstr(vs.Object, "spec", "source", "persistentVolumeClaimName")
	srcContent := nstr(vs.Object, "spec", "source", "volumeSnapshotContentName")
	if srcPVC != "" {
		node.Attr("sourcePVC", srcPVC)
		if expectPVC != "" && srcPVC != expectPVC {
			node.Escalate(tree.StateWarn)
			r.rep.Problem(tree.StateWarn, where,
				fmt.Sprintf("VolumeSnapshot sources PVC %q but the volumeBackup recorded %q", srcPVC, expectPVC),
				"the snapshot may not contain the disk you think it does")
		}
	}
	if srcContent != "" {
		node.Attr("preProvisionedContent", srcContent)
	}

	// Under --trim-repeats, stop here if this exact snapshot's content chain has
	// already been written out in full earlier in this report. The attributes above
	// are still shown, so the node never becomes a dead end.
	if r.opt.TrimRepeats && r.alreadyRendered("VolumeSnapshot", vs.GetNamespace(), vs.GetName()) {
		node.Note("content chain already shown above (hidden by --trim-repeats)")
		return
	}

	contentName := nstr(vs.Object, "status", "boundVolumeSnapshotContentName")
	contentObj, contentNode := r.getOrNode(ctx,
		source.Ref{Kind: "VolumeSnapshotContent", Name: contentName},
		where+" -> status.boundVolumeSnapshotContentName")
	node.Add(contentNode)
	if contentObj == nil {
		return
	}
	r.describeVolumeSnapshotContent(ctx, contentObj, contentNode, vs)
}

func (r *Resolver) describeVolumeSnapshotContent(ctx context.Context, vsc *unstructured.Unstructured, node *tree.Node, vs *unstructured.Unstructured) {
	where := "VolumeSnapshotContent/" + vsc.GetName()

	node.Attr("driver", nstr(vsc.Object, "spec", "driver"))
	node.Attr("ready", boolStr(vsc.Object, "status", "readyToUse"))
	node.Attr("restoreSize", nqty(vsc.Object, "status", "restoreSize"))
	node.Attr("deletionPolicy", nstr(vsc.Object, "spec", "deletionPolicy"))
	node.Attr("sourceVolumeMode", nstr(vsc.Object, "spec", "sourceVolumeMode"))

	if msg := errMessage(vsc.Object, "status", "error"); msg != "" {
		node.Escalate(tree.StateError)
		node.Note("error: " + msg)
		r.rep.Problem(tree.StateError, where, "VolumeSnapshotContent error: "+msg, "")
	}

	// Validate the back-reference to the VolumeSnapshot. A mismatched
	// volumeSnapshotRef is why a snapshot can sit un-bound forever.
	refName := nstr(vsc.Object, "spec", "volumeSnapshotRef", "name")
	refNS := nstr(vsc.Object, "spec", "volumeSnapshotRef", "namespace")
	if refName != "" && (refName != vs.GetName() || (refNS != "" && refNS != vs.GetNamespace())) {
		node.Escalate(tree.StateError)
		r.rep.Problem(tree.StateError, where,
			fmt.Sprintf("spec.volumeSnapshotRef points at %s/%s but this content is bound from %s/%s",
				refNS, refName, vs.GetNamespace(), vs.GetName()),
			"a crossed or stale volumeSnapshotRef; the pair will never reconcile")
	}

	// The source volume handle is the array volume the snapshot was taken from --
	// exactly what you search for on the array side, and the only pointer to it
	// once the source PVC has been deleted.
	sourceVolumeHandle := nstr(vsc.Object, "spec", "source", "volumeHandle")
	if sourceVolumeHandle != "" {
		node.Attr("sourceVolumeHandle", sourceVolumeHandle)
	}
	if sh := nstr(vsc.Object, "spec", "source", "snapshotHandle"); sh != "" {
		node.Attr("spec.snapshotHandle", sh)
	}

	handle := nstr(vsc.Object, "status", "snapshotHandle")
	if handle == "" {
		node.Escalate(tree.StateMissing)
		node.Note("status.snapshotHandle is empty: nothing exists on the array for this snapshot")
		r.rep.Problem(tree.StateMissing, where, "no snapshotHandle",
			"the CSI CreateSnapshot call never succeeded; check the csi-snapshotter sidecar logs for this driver")
		return
	}
	node.Attr("snapshotHandle", handle)

	if r.opt.SkipArray {
		return
	}
	r.addArrayObject(ctx, node, handle, arrayLookup{
		kind:        r.opt.ArraySnapshotKind,
		spec:        arraySnapshotFields,
		checkParent: true,
	})

	// Also resolve the volume the snapshot was carved from. It often no longer
	// exists, so this is reported as lineage rather than as a fault.
	if sourceVolumeHandle != "" && sourceVolumeHandle != handle {
		r.addArrayObject(ctx, node, sourceVolumeHandle, arrayLookup{
			kind:   r.opt.ArrayVolumeKind,
			spec:   arrayVolumeFields,
			prefix: "origin ",
			soft:   true,
		})
	}
}

// addRestores lists VirtualMachineRestores consuming this snapshot. These are
// what create new PVCs, so they matter when a restore left PVCs missing.
func (r *Resolver) addRestores(ctx context.Context, ns, snapName string, node *tree.Node) {
	for _, obj := range r.listKind(ctx, "VirtualMachineRestore", ns) {
		if nstr(obj.Object, "spec", "virtualMachineSnapshotName") != snapName {
			continue
		}
		where := "VirtualMachineRestore/" + obj.GetName()
		n := node.Add(tree.New("↳ "+where).
			Object("VirtualMachineRestore", ns, obj.GetName(), string(obj.GetUID())).
			SetState(tree.StateInfo))
		n.Attr("target", nstr(obj.Object, "spec", "target", "name"))
		n.Attr("restoreTime", nstr(obj.Object, "status", "restoreTime"))
		// These policies decide whether a failed restore cleans up after itself:
		// targetReadinessPolicy defaults to WaitGracePeriod (a 5m deadline), and
		// volumeOwnershipPolicy None leaves restored PVCs owned by nothing.
		n.Attr("targetReadinessPolicy", nstr(obj.Object, "spec", "targetReadinessPolicy"))
		n.Attr("volumeRestorePolicy", nstr(obj.Object, "spec", "volumeRestorePolicy"))
		n.Attr("volumeOwnershipPolicy", nstr(obj.Object, "spec", "volumeOwnershipPolicy"))

		complete, hasComplete := nbool(obj.Object, "status", "complete")
		n.Attr("complete", boolStr(obj.Object, "status", "complete"))

		// A Failure condition explains an incomplete restore precisely, so report
		// that instead of also emitting the generic "did not complete" guess --
		// duplicate entries for one fault just dilute the problem list.
		failure := conditionMessage(obj.Object, "Failure", "True")
		age := ageString(obj.GetCreationTimestamp().Time)

		switch {
		case failure != "":
			n.Escalate(tree.StateError)
			n.Note("condition Failure=True: " + failure)
			r.rep.Problem(tree.StateError, where,
				fmt.Sprintf("restore failed after %s: %s", age, failure),
				"a Failure condition does not stop the controller reconciling, so check the restored-PVC state below before assuming nothing happened; to retry, power off the target VM and recreate this VirtualMachineRestore")
		case !hasComplete || !complete:
			// Incomplete with no Failure yet: still in flight, or wedged silently.
			n.Escalate(tree.StateWarn)
			n.Note("incomplete, created " + age + " ago")
			r.rep.Problem(tree.StateWarn, where,
				fmt.Sprintf("restore has not completed (age %s)", age),
				r.stalledRestoreHint(ctx, ns, obj))
		}

		if msg := errMessage(obj.Object, "status", "error"); msg != "" {
			n.Escalate(tree.StateError)
			n.Note("error: " + msg)
			r.rep.Problem(tree.StateError, where, "restore error: "+msg, "")
		}
		if msg := conditionMessage(obj.Object, "Progressing", "True"); msg != "" {
			n.Note("condition Progressing=True: " + msg)
		}
		// Ready=False restates the Failure message verbatim in KubeVirt, so only
		// show it when it adds something.
		if msg := conditionMessage(obj.Object, "Ready", "False"); msg != "" && msg != failure {
			n.Note("condition Ready=False: " + msg)
		}

		// restores[] maps each VM disk to the VolumeSnapshot it was restored from
		// and the new PVC that was created for it.
		for _, raw := range nslice(obj.Object, "status", "restores") {
			m := asMap(raw)
			// json tags are volumeName / volumeSnapshotName / persistentVolumeClaim
			// (not persistentVolumeClaimName) / dataVolumeName.
			newPVC := nstr(m, "persistentVolumeClaim")
			line := fmt.Sprintf("disk %s: from VolumeSnapshot %s into new PVC %s",
				nstr(m, "volumeName"), nstr(m, "volumeSnapshotName"), newPVC)
			if dv := nstr(m, "dataVolumeName"); dv != "" {
				line += " via DataVolume " + dv
			}

			// Two opposite faults live here. A complete restore whose PVC is gone is
			// a silently lost disk. An incomplete restore whose PVC exists is an
			// orphan: provisioned array capacity that nothing will ever consume.
			var orphan *unstructured.Unstructured
			if newPVC != "" {
				pvcObj, err := r.src.Get(ctx, source.Ref{Kind: "PersistentVolumeClaim", Namespace: ns, Name: newPVC})
				switch {
				case err == nil && hasComplete && complete:
					line += " [PVC exists]"
				case err == nil:
					// Created, but the restore never finished. Two independent
					// questions matter here, and conflating them misleads: is any
					// array capacity actually allocated (only if it is bound to a PV),
					// and will anything ever clean it up (only if it is owned).
					phase := nstr(pvcObj.Object, "status", "phase")
					allocated := nstr(pvcObj.Object, "spec", "volumeName") != ""
					capacity := "unbound, so no array volume has been provisioned for it yet"
					if allocated {
						capacity = "bound to a PersistentVolume, so array capacity is allocated"
					}

					owners := pvcObj.GetOwnerReferences()
					if len(owners) == 0 {
						line += fmt.Sprintf(" [PVC exists, phase %s, UNOWNED]", phase)
						n.Escalate(tree.StateWarn)
						r.rep.Problem(tree.StateWarn, where,
							fmt.Sprintf("incomplete restore left PVC %q behind (phase %s, %s) with no ownerReferences", newPVC, phase, capacity),
							"nothing will garbage-collect it -- delete the PVC directly once the restore is abandoned (volumeOwnershipPolicy None creates unowned volumes)")
					} else {
						line += fmt.Sprintf(" [PVC exists, phase %s, owned by %s/%s]", phase, owners[0].Kind, owners[0].Name)
						n.Escalate(tree.StateWarn)
						r.rep.Problem(tree.StateWarn, where,
							fmt.Sprintf("incomplete restore has already created PVC %q (phase %s, %s)", newPVC, phase, capacity),
							fmt.Sprintf("it is owned by %s/%s, so deleting the VirtualMachineRestore will not remove it; delete the PVC directly once the restore is abandoned",
								owners[0].Kind, owners[0].Name))
					}
					orphan = pvcObj
				case hasComplete && complete:
					line += " [PVC MISSING]"
					n.Escalate(tree.StateMissing)
					r.rep.Problem(tree.StateMissing, where,
						fmt.Sprintf("restore reports complete but its restored PVC %q does not exist", newPVC),
						"the disk this restore created has since been deleted; the VM cannot start from it")
				default:
					line += " [PVC not created yet]"
				}
			}
			n.Note(line)

			// Resolve the stray volume down to the array so it can be reclaimed,
			// at full depth like any other PVC.
			if orphan != nil {
				r.describePVC(ctx, ns, newPVC, n, 0, pvcContext{stray: true})
			}
		}
		r.rep.At(createdAt(obj), "VirtualMachineRestore "+obj.GetName()+" created", tree.StateInfo)
		if rt := nstr(obj.Object, "status", "restoreTime"); rt != "" {
			r.rep.At(rt, "VirtualMachineRestore "+obj.GetName()+" completed", tree.StateInfo)
		} else if failure != "" {
			when := conditionTime(obj.Object, "Failure", "True")
			if when == "" {
				when = createdAt(obj)
			}
			r.rep.At(when, "VirtualMachineRestore "+obj.GetName()+" FAILED", tree.StateError)
		}
		for _, d := range stringSlice(nslice(obj.Object, "status", "deletedDataVolumes")) {
			n.Note("deleted DataVolume during restore: " + d)
		}
	}
}

// stalledRestoreHint explains an incomplete restore using the target VM's actual
// state. Advising "stop the target VM" when it is already stopped sends the reader
// in the wrong direction, so establish that first.
func (r *Resolver) stalledRestoreHint(ctx context.Context, ns string, restore *unstructured.Unstructured) string {
	target := nstr(restore.Object, "spec", "target", "name")
	if target == "" {
		return "check the restore's Progressing condition and the events on the PVCs it is creating"
	}
	vm, err := r.src.Get(ctx, source.Ref{Kind: "VirtualMachine", Namespace: ns, Name: target})
	if err != nil {
		return fmt.Sprintf("target VM %q could not be read; check its run state and the events on the PVCs this restore is creating", target)
	}
	status := nstr(vm.Object, "status", "printableStatus")
	switch status {
	case "Stopped", "Halted":
		return fmt.Sprintf("target VM %s is already %s, so target readiness is NOT the blocker -- the Progressing condition and the events on the restored PVC are the real cause",
			target, status)
	case "":
		return "check the restore's Progressing condition and the events on the PVCs it is creating"
	default:
		return fmt.Sprintf("target VM %s is %s: KubeVirt will not restore into a running VM, so stop it and re-check this restore",
			target, status)
	}
}

// ageString renders a duration compactly for "stuck for how long" reporting.
func ageString(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// addProducers answers "what created this snapshot" by walking ownerReferences
// upward and by scanning VirtualMachineClone status. There is no single fixed
// parent kind, unlike DataVolume -> PVC, so both angles are needed.
func (r *Resolver) addProducers(ctx context.Context, obj *unstructured.Unstructured, node *tree.Node) {
	for _, ref := range obj.GetOwnerReferences() {
		n := node.Add(tree.New(fmt.Sprintf("⤴ owner %s/%s", ref.Kind, ref.Name)).SetState(tree.StateInfo))
		owner, err := r.src.Get(ctx, source.Ref{Kind: ref.Kind, Namespace: obj.GetNamespace(), Name: ref.Name})
		if err != nil {
			n.Note("owner object not retrievable: " + err.Error())
			continue
		}
		n.Attr("phase", joinNonEmpty("/", nstr(owner.Object, "status", "phase"), nstr(owner.Object, "status", "printableStatus")))
		if len(owner.GetOwnerReferences()) > 0 {
			r.addProducers(ctx, owner, n)
		}
	}
	// A VirtualMachineClone records the snapshot it created without owning it in
	// every version, so match on status.snapshotName too.
	for _, c := range r.listKind(ctx, "VirtualMachineClone", obj.GetNamespace()) {
		if nstr(c.Object, "status", "snapshotName") != obj.GetName() {
			continue
		}
		n := node.Add(tree.New("⤴ VirtualMachineClone/" + c.GetName()).SetState(tree.StateInfo))
		n.Attr("phase", nstr(c.Object, "status", "phase"))
		n.Attr("target", nstr(c.Object, "status", "targetName"))
		n.Attr("restore", nstr(c.Object, "status", "restoreName"))
		n.Note("this snapshot is an intermediate object of a clone operation")
	}
}

func indications(o map[string]interface{}) string {
	var out []string
	for _, key := range []string{"sourceIndications", "indications"} {
		for _, raw := range nslice(o, "status", key) {
			if s, ok := raw.(string); ok {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			break
		}
	}
	return strings.Join(out, ",")
}

// volumeLists surfaces status.snapshotVolumes, which names the disks that were
// deliberately excluded -- a very common reason a "missing" PVC was never
// snapshotted in the first place.
func volumeLists(o map[string]interface{}) string {
	inc := stringSlice(nslice(o, "status", "snapshotVolumes", "includedVolumes"))
	exc := stringSlice(nslice(o, "status", "snapshotVolumes", "excludedVolumes"))
	var parts []string
	if len(inc) > 0 {
		parts = append(parts, "included: "+strings.Join(inc, ","))
	}
	if len(exc) > 0 {
		parts = append(parts, "EXCLUDED: "+strings.Join(exc, ","))
	}
	return strings.Join(parts, "  ")
}

func stringSlice(raw []interface{}) []string {
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
