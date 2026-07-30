package resolve

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/mjovanovic0/kubectl-vmsnap/internal/tree"
)

// addVolumeAttachments reports where a PV is currently attached.
//
// A volume still attached to a node that no longer runs the VM is the classic
// cause of "Multi-Attach error for volume" and of a VM that will not start
// elsewhere. The node name is also what you correlate against array-side host
// connections.
func (r *Resolver) addVolumeAttachments(ctx context.Context, pvName string, node *tree.Node) {
	vas := r.listKind(ctx, "VolumeAttachment", "")
	if vas == nil {
		return
	}
	var found int
	for _, va := range vas {
		if nstr(va.Object, "spec", "source", "persistentVolumeName") != pvName {
			continue
		}
		found++
		attached, _ := nbool(va.Object, "status", "attached")
		n := node.Add(tree.New("VolumeAttachment/"+va.GetName()).
			Object("VolumeAttachment", "", va.GetName(), string(va.GetUID())))
		n.Attr("node", nstr(va.Object, "spec", "nodeName"))
		n.Attr("attached", fieldStr(va.Object, "status", "attached"))
		n.Attr("attacher", nstr(va.Object, "spec", "attacher"))
		if !attached {
			n.Escalate(tree.StateWarn)
			n.Note("attachment recorded but not completed")
		}
		if va.GetDeletionTimestamp() != nil {
			n.Escalate(tree.StateWarn)
			n.Note("detaching since " + va.GetDeletionTimestamp().String())
		}
		if msg := nstr(va.Object, "status", "detachError", "message"); msg != "" {
			n.Escalate(tree.StateError)
			n.Note("detachError: " + msg)
			r.rep.Problem(tree.StateError, "VolumeAttachment/"+va.GetName(),
				"detach failed: "+msg,
				"the volume stays attached to the old node and no other node can use it; this is what produces Multi-Attach errors")
		}
		if msg := nstr(va.Object, "status", "attachError", "message"); msg != "" {
			n.Escalate(tree.StateError)
			n.Note("attachError: " + msg)
			r.rep.Problem(tree.StateError, "VolumeAttachment/"+va.GetName(),
				"attach failed: "+msg,
				"check the CSI node plugin logs on this node and the array-side host connection")
		}
	}
	if found > 1 {
		node.Escalate(tree.StateWarn)
		r.rep.Problem(tree.StateWarn, "PersistentVolume/"+pvName,
			fmt.Sprintf("%d VolumeAttachments reference this PV", found),
			"the volume is attached to more than one node; for a ReadWriteOnce volume this blocks scheduling")
	}
	if found == 0 {
		node.Note("no VolumeAttachment: not attached to any node right now")
	}
}

// addPVCConsumers reports who else is using a PVC.
//
// "Why will my VM not start" is very often another pod still holding the volume,
// and it is also why a PVC will not finish terminating.
func (r *Resolver) addPVCConsumers(ctx context.Context, pvc *unstructured.Unstructured, node *tree.Node) {
	ns := pvc.GetNamespace()
	name := pvc.GetName()

	pods := r.listKind(ctx, "Pod", ns)
	if pods == nil {
		return
	}
	var users []string
	for _, pod := range pods {
		if !podUsesPVC(pod, name) {
			continue
		}
		users = append(users, fmt.Sprintf("%s (%s on %s)",
			pod.GetName(),
			orDash(nstr(pod.Object, "status", "phase")),
			orDash(nstr(pod.Object, "spec", "nodeName"))))
	}
	if len(users) == 0 {
		node.Note("no pod currently mounts this PVC")
		return
	}
	node.Note("mounted by: " + strings.Join(users, ", "))

	// More than one live consumer of a single-writer volume is a real conflict.
	modes := stringSlice(nslice(pvc.Object, "spec", "accessModes"))
	singleWriter := len(modes) > 0 && modes[0] == "ReadWriteOnce"
	if singleWriter && len(users) > 1 {
		node.Escalate(tree.StateWarn)
		r.rep.Problem(tree.StateWarn, "PersistentVolumeClaim/"+name,
			fmt.Sprintf("%d pods use this ReadWriteOnce PVC", len(users)),
			"only one can mount it; the others stay Pending with a Multi-Attach or FailedMount error")
	}
	if pvc.GetDeletionTimestamp() != nil && len(users) > 0 {
		r.rep.Problem(tree.StateWarn, "PersistentVolumeClaim/"+name,
			"PVC is terminating while still mounted",
			"the pvc-protection finalizer holds it until every consuming pod is gone")
	}
}

func podUsesPVC(pod *unstructured.Unstructured, pvcName string) bool {
	for _, raw := range nslice(pod.Object, "spec", "volumes") {
		v := asMap(raw)
		if nstr(v, "persistentVolumeClaim", "claimName") == pvcName {
			return true
		}
	}
	return false
}

// checkCapacityConsistency compares the sizes along a restore path. A snapshot
// larger than the PVC it is being restored into cannot bind, and the resulting
// error surfaces far from the cause.
func (r *Resolver) checkCapacityConsistency(pvcRequest, pvCapacity, snapshotSize string, where string, node *tree.Node) {
	req := parseQuantityBytes(pvcRequest)
	cap := parseQuantityBytes(pvCapacity)
	snap := parseQuantityBytes(snapshotSize)

	if req > 0 && snap > 0 && snap > req {
		node.Escalate(tree.StateWarn)
		node.Note(fmt.Sprintf("snapshot restoreSize %s exceeds the PVC request %s", snapshotSize, pvcRequest))
		r.rep.Problem(tree.StateWarn, where,
			fmt.Sprintf("snapshot is %s but the target PVC only requests %s", snapshotSize, pvcRequest),
			"a restore into a smaller volume is rejected by the CSI driver; raise the PVC request to at least the snapshot size")
	}
	if req > 0 && cap > 0 && cap < req {
		node.Escalate(tree.StateWarn)
		node.Note(fmt.Sprintf("bound PV capacity %s is below the request %s", pvCapacity, pvcRequest))
		r.rep.Problem(tree.StateWarn, where,
			fmt.Sprintf("PV provides %s but the PVC requested %s", pvCapacity, pvcRequest),
			"the guest may see less space than expected; check whether an expansion was started and never completed")
	}
}

// parseQuantityBytes converts the binary-unit strings this tool prints back into
// bytes for comparison. Returns 0 when it cannot tell, so callers stay silent
// rather than guessing.
func parseQuantityBytes(q string) int64 {
	q = strings.TrimSpace(q)
	if q == "" {
		return 0
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"Pi", 1 << 50}, {"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10},
	}
	for _, u := range units {
		if strings.HasSuffix(q, u.suffix) {
			var v float64
			if _, err := fmt.Sscanf(strings.TrimSuffix(q, u.suffix), "%g", &v); err != nil {
				return 0
			}
			return int64(v * float64(u.mult))
		}
	}
	var v int64
	if _, err := fmt.Sscanf(q, "%d", &v); err != nil {
		return 0
	}
	return v
}
