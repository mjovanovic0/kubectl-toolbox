package resolve

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/mjovanovic0/kubectl-vmstorage/internal/source"
	"github.com/mjovanovic0/kubectl-vmstorage/internal/tree"
)

// addRuntime reports what the VM actually has attached right now, as opposed to
// what its spec asks for.
//
// The VirtualMachineInstance is the authority on the live disks: status.volumeStatus
// gives a per-disk phase and reason, which is where hotplug failures and
// unmountable volumes appear. The virt-launcher pod underneath it is where mount
// errors surface. Neither is reachable from the spec-side chain, and both are
// where you look when the storage objects all appear healthy but the VM will not
// run.
func (r *Resolver) addRuntime(ctx context.Context, vm *unstructured.Unstructured, parent *tree.Node) {
	ns := vm.GetNamespace()
	name := vm.GetName()

	vmi, err := r.src.Get(ctx, source.Ref{Kind: "VirtualMachineInstance", Namespace: ns, Name: name})
	if err != nil {
		// Not running is a legitimate state, not a fault. But if the VM claims to
		// be running, the missing VMI is the story.
		printable := nstr(vm.Object, "status", "printableStatus")
		n := parent.Add(tree.New("runtime").SetState(tree.StateInfo))
		if printable == "Running" {
			n.Escalate(tree.StateWarn)
			n.Note("VM reports Running but no VirtualMachineInstance exists")
			r.rep.Problem(tree.StateWarn, "VirtualMachine/"+name,
				"printableStatus is Running but there is no VirtualMachineInstance",
				"the VMI was deleted underneath the VM, or the virt-controller is not reconciling")
			return
		}
		n.Note("no VirtualMachineInstance: the VM is not running, so nothing is attached")
		return
	}

	node := parent.Add(tree.New("runtime VirtualMachineInstance/"+vmi.GetName()).
		Object("VirtualMachineInstance", ns, vmi.GetName(), string(vmi.GetUID())))
	phase := nstr(vmi.Object, "status", "phase")
	node.Attr("phase", phase)
	node.Attr("node", nstr(vmi.Object, "status", "nodeName"))
	node.Attr("started", nstr(vmi.Object, "status", "phaseTransitionTimestamps"))
	if phase != "Running" && phase != "" {
		node.Escalate(tree.StateWarn)
		r.rep.Problem(tree.StateWarn, "VirtualMachineInstance/"+vmi.GetName(),
			fmt.Sprintf("VMI phase is %q", phase),
			"a VMI stuck in Scheduling or Pending is usually waiting on a volume; check the volumeStatus entries and the virt-launcher pod below")
	}
	if reason := nstr(vmi.Object, "status", "reason"); reason != "" {
		node.Note("reason: " + reason)
	}
	for _, ct := range []string{"Ready", "Paused", "LiveMigratable", "StorageLiveMigratable"} {
		if msg := conditionMessage(vmi.Object, ct, "False"); msg != "" {
			node.Note("condition " + ct + "=False: " + msg)
		}
	}
	r.rep.At(nstr(vmi.Object, "status", "phaseTransitionTimestamps"), "VMI "+vmi.GetName()+" phase "+phase, tree.StateInfo)

	r.addVolumeStatuses(vmi, node)
	r.addMigrationState(vmi, node)
	r.addLauncherPod(ctx, ns, vmi, node)
}

// addVolumeStatuses reports the per-disk runtime state the VMI publishes.
func (r *Resolver) addVolumeStatuses(vmi *unstructured.Unstructured, node *tree.Node) {
	statuses := nslice(vmi.Object, "status", "volumeStatus")
	if len(statuses) == 0 {
		return
	}
	for _, raw := range statuses {
		vs := asMap(raw)
		volName := nstr(vs, "name")
		n := node.Add(tree.New("attached[" + volName + "]").SetState(tree.StateInfo))
		n.Attr("target", nstr(vs, "target"))
		n.Attr("phase", nstr(vs, "phase"))
		n.Attr("claim", nstr(vs, "persistentVolumeClaimInfo", "claimName"))
		if fs := fieldStr(vs, "persistentVolumeClaimInfo", "filesystemOverhead"); fs != "" {
			n.Attr("fsOverhead", fs)
		}
		if mode := nstr(vs, "persistentVolumeClaimInfo", "volumeMode"); mode != "" {
			n.Attr("volumeMode", mode)
		}
		if reason := nstr(vs, "reason"); reason != "" {
			n.Note("reason: " + reason)
		}
		if msg := nstr(vs, "message"); msg != "" {
			n.Note("message: " + msg)
		}
		// Hotplug state, when this disk was added to a running VM.
		if hp := nmap(vs, "hotplugVolume"); len(hp) > 0 {
			n.Note(fmt.Sprintf("hotplugged: attachPod=%s podUID=%s",
				orDash(nstr(hp, "attachPodName")), orDash(nstr(hp, "attachPodUID"))))
		}
		phase := nstr(vs, "phase")
		if phase != "" && phase != "Ready" {
			n.Escalate(tree.StateWarn)
			r.rep.Problem(tree.StateWarn, "VirtualMachineInstance/"+vmi.GetName(),
				fmt.Sprintf("disk %q is %q, not Ready: %s", volName, phase,
					joinNonEmpty(" ", nstr(vs, "reason"), nstr(vs, "message"))),
				"this is the live view of the disk; a spec-side chain that looks healthy can still fail to attach")
		}
	}
}

// addMigrationState surfaces an in-flight or failed live migration, which changes
// which node holds the volumes and can leave attachments behind.
func (r *Resolver) addMigrationState(vmi *unstructured.Unstructured, node *tree.Node) {
	ms := nmap(vmi.Object, "status", "migrationState")
	if len(ms) == 0 {
		return
	}
	n := node.Add(tree.New("migration").SetState(tree.StateInfo))
	n.Attr("sourceNode", nstr(ms, "sourceNode"))
	n.Attr("targetNode", nstr(ms, "targetNode"))
	n.Attr("completed", fieldStr(ms, "completed"))
	n.Attr("failed", fieldStr(ms, "failed"))
	if failed, _ := nbool(ms, "failed"); failed {
		n.Escalate(tree.StateWarn)
		r.rep.Problem(tree.StateWarn, "VirtualMachineInstance/"+vmi.GetName(),
			"the last live migration failed",
			"volumes may still be attached to the source node; check the VolumeAttachments above")
	}
}

// addLauncherPod finds the virt-launcher pod and reports why it is not running.
// Mount failures appear here and nowhere else in the chain.
func (r *Resolver) addLauncherPod(ctx context.Context, ns string, vmi *unstructured.Unstructured, node *tree.Node) {
	pods := r.listKind(ctx, "Pod", ns)
	if pods == nil {
		return
	}
	vmiUID := string(vmi.GetUID())
	for _, pod := range pods {
		if !isLauncherFor(pod, vmi.GetName(), vmiUID) {
			continue
		}
		phase := nstr(pod.Object, "status", "phase")
		n := node.Add(tree.New("virt-launcher Pod/"+pod.GetName()).
			Object("Pod", ns, pod.GetName(), string(pod.GetUID())))
		n.Attr("phase", phase)
		n.Attr("node", nstr(pod.Object, "spec", "nodeName"))
		if phase != "Running" && phase != "Succeeded" {
			n.Escalate(tree.StateWarn)
		}
		// Unschedulable / volume-mount failures live in the pod conditions.
		for _, raw := range nslice(pod.Object, "status", "conditions") {
			c := asMap(raw)
			if nstr(c, "status") != "False" {
				continue
			}
			text := joinNonEmpty(": ", nstr(c, "reason"), nstr(c, "message"))
			if text == "" {
				continue
			}
			n.Note("condition " + nstr(c, "type") + "=False: " + text)
			if strings.Contains(strings.ToLower(text), "volume") ||
				nstr(c, "reason") == "Unschedulable" {
				n.Escalate(tree.StateWarn)
				r.rep.Problem(tree.StateWarn, "Pod/"+pod.GetName(),
					"virt-launcher cannot start: "+text,
					"this is the storage failure as the kubelet sees it, and it names the volume at fault")
			}
		}
		// Container-level waiting reasons (ImagePullBackOff, CreateContainerError).
		for _, key := range []string{"containerStatuses", "initContainerStatuses"} {
			for _, raw := range nslice(pod.Object, "status", key) {
				cs := asMap(raw)
				if reason := nstr(cs, "state", "waiting", "reason"); reason != "" {
					n.Escalate(tree.StateWarn)
					n.Note(fmt.Sprintf("container %s waiting: %s %s",
						nstr(cs, "name"), reason, truncate(nstr(cs, "state", "waiting", "message"), 160)))
				}
				if reason := nstr(cs, "lastState", "terminated", "reason"); reason != "" {
					n.Note(fmt.Sprintf("container %s last terminated: %s (exit %s)",
						nstr(cs, "name"), reason, fieldStr(cs, "lastState", "terminated", "exitCode")))
				}
			}
		}
		return
	}
	node.Note("no virt-launcher pod found for this VMI")
}

// isLauncherFor matches the virt-launcher pod of a VMI. The created-by label is
// authoritative; the name prefix is the fallback for older versions.
func isLauncherFor(pod *unstructured.Unstructured, vmiName, vmiUID string) bool {
	labels := pod.GetLabels()
	if labels["kubevirt.io"] != "virt-launcher" {
		return false
	}
	if uid := labels["kubevirt.io/created-by"]; uid != "" && vmiUID != "" {
		return uid == vmiUID
	}
	return strings.HasPrefix(pod.GetName(), "virt-launcher-"+vmiName+"-")
}
