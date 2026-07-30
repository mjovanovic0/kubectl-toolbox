package resolve

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/mjovanovic0/kubectl-vmsnap/internal/source"
	"github.com/mjovanovic0/kubectl-vmsnap/internal/tree"
)

// maxProvenanceDepth bounds the walk up import lineages, which can chain
// snapshot -> PVC -> snapshot indefinitely across golden-image generations.
const maxProvenanceDepth = 4

// Disks resolves the live storage chain of a VM:
//
//	VirtualMachine
//	  -> DataVolume            (spec.sourceRef -> DataSource <- DataImportCron)
//	  -> PersistentVolumeClaim (spec.volumeName)
//	  -> PersistentVolume      (spec.csi.volumeHandle)
//	  -> vendor array CR       (PureVolume)
func (r *Resolver) Disks(ctx context.Context, vmName string) *tree.Report {
	ns := r.opt.Namespace
	r.beginReport()

	vm, vmNode := r.getOrNode(ctx, source.Ref{Kind: "VirtualMachine", Namespace: ns, Name: vmName}, "VirtualMachine lookup")
	r.rep.Root = vmNode
	if vm == nil {
		return r.finish(ctx)
	}
	r.describeVMNode(vm, vmNode)
	r.addRuntime(ctx, vm, vmNode)
	r.addDisks(ctx, vm, vmNode)
	return r.finish(ctx)
}

// describeVMNode annotates a VM node with its run state.
func (r *Resolver) describeVMNode(vm *unstructured.Unstructured, node *tree.Node) {
	node.Attr("printableStatus", nstr(vm.Object, "status", "printableStatus"))
	if b, found := nbool(vm.Object, "spec", "running"); found {
		node.Attr("running", fmt.Sprintf("%t", b))
	}
	node.Attr("runStrategy", nstr(vm.Object, "spec", "runStrategy"))
}

// addDisks resolves every volume of a VM under the given node. Split out from
// Disks so the combined `vm` view can nest it under a heading.
func (r *Resolver) addDisks(ctx context.Context, vm *unstructured.Unstructured, node *tree.Node) {
	ns := vm.GetNamespace()

	// dataVolumeTemplates are the DVs this VM owns and expects to exist.
	templates := map[string]bool{}
	for _, raw := range nslice(vm.Object, "spec", "dataVolumeTemplates") {
		if n := nstr(asMap(raw), "metadata", "name"); n != "" {
			templates[n] = true
		}
	}

	volumes := nslice(vm.Object, "spec", "template", "spec", "volumes")
	if len(volumes) == 0 {
		node.Note("VM template declares no volumes")
	}
	for _, raw := range volumes {
		r.describeVMVolume(ctx, ns, asMap(raw), node, templates)
	}

	// A template whose DataVolume is absent is a concrete, actionable failure.
	// Iterate in sorted order so output is stable across runs.
	for _, name := range sortedKeys(templates) {
		if _, err := r.src.Get(ctx, source.Ref{Kind: "DataVolume", Namespace: ns, Name: name}); err != nil {
			n := node.Add(tree.New("dataVolumeTemplate[" + name + "]").Escalate(tree.StateWarn))
			n.Note("declared in spec.dataVolumeTemplates but no DataVolume exists")
			r.rep.Problem(tree.StateWarn, "VirtualMachine/"+vm.GetName(),
				fmt.Sprintf("dataVolumeTemplate %q has no DataVolume", name),
				"CDI garbage-collects completed DataVolumes when dataVolumeTTLSeconds is set; verify the PVC of the same name exists instead")
		}
	}
}

// describeVMVolume dispatches on the volume source type declared in the VM.
func (r *Resolver) describeVMVolume(ctx context.Context, ns string, vol map[string]interface{}, parent *tree.Node, templates map[string]bool) {
	diskName := nstr(vol, "name")

	switch {
	case nstr(vol, "dataVolume", "name") != "":
		dvName := nstr(vol, "dataVolume", "name")
		delete(templates, dvName)
		node := parent.Add(tree.New("volume[" + diskName + "] -> DataVolume/" + dvName))
		r.describeDataVolume(ctx, ns, dvName, node, 0)

	case nstr(vol, "persistentVolumeClaim", "claimName") != "":
		pvcName := nstr(vol, "persistentVolumeClaim", "claimName")
		node := parent.Add(tree.New("volume[" + diskName + "] -> PVC/" + pvcName))
		r.describePVC(ctx, ns, pvcName, node, 0, pvcContext{})

	case nstr(vol, "memoryDump", "claimName") != "":
		pvcName := nstr(vol, "memoryDump", "claimName")
		node := parent.Add(tree.New("volume[" + diskName + "] -> memoryDump PVC/" + pvcName))
		r.describePVC(ctx, ns, pvcName, node, 0, pvcContext{})

	case nstr(vol, "ephemeral", "persistentVolumeClaim", "claimName") != "":
		pvcName := nstr(vol, "ephemeral", "persistentVolumeClaim", "claimName")
		node := parent.Add(tree.New("volume[" + diskName + "] -> ephemeral PVC/" + pvcName).SetState(tree.StateInfo))
		node.Note("ephemeral: writes go to a local overlay and are discarded")
		r.describePVC(ctx, ns, pvcName, node, 0, pvcContext{})

	case nstr(vol, "containerDisk", "image") != "":
		parent.Add(tree.New("volume["+diskName+"] -> containerDisk").SetState(tree.StateInfo)).
			Attr("image", nstr(vol, "containerDisk", "image")).
			Note("ephemeral container image, no PVC and no snapshot")

	default:
		// cloudInit, configMap, secret, serviceAccount, emptyDisk, sysprep...
		kinds := make([]string, 0, len(vol))
		for k := range vol {
			if k != "name" {
				kinds = append(kinds, k)
			}
		}
		parent.Add(tree.New("volume["+diskName+"]").SetState(tree.StateInfo)).
			Attr("type", strings.Join(kinds, ",")).
			Note("not backed by persistent storage")
	}
}

func (r *Resolver) describeDataVolume(ctx context.Context, ns, name string, node *tree.Node, depth int) {
	dv, err := r.src.Get(ctx, source.Ref{Kind: "DataVolume", Namespace: ns, Name: name})
	if err != nil {
		// Do not call this broken: CDI deletes succeeded DataVolumes when
		// dataVolumeTTLSeconds is set, and the PVC legitimately outlives them.
		node.SetState(tree.StateInfo)
		node.Note("no DataVolume object (normal after CDI garbage collection); resolving PVC of the same name")
		r.describePVC(ctx, ns, name, node, depth, pvcContext{})
		return
	}

	where := "DataVolume/" + name
	phase := nstr(dv.Object, "status", "phase")
	node.Attr("dvPhase", phase)
	node.Attr("progress", nstr(dv.Object, "status", "progress"))
	if rc := fieldStr(dv.Object, "status", "restartCount"); rc != "" && rc != "0" {
		node.Attr("restarts", rc)
		node.Escalate(tree.StateWarn)
		r.rep.Problem(tree.StateWarn, where, "importer pod has restarted "+rc+" times",
			"check the importer pod logs in this namespace for the underlying failure")
	}
	if phase != "" && phase != "Succeeded" && phase != "PendingPopulation" && phase != "WaitForFirstConsumer" {
		node.Escalate(tree.StateWarn)
		r.rep.Problem(tree.StateWarn, where, fmt.Sprintf("DataVolume phase is %q", phase),
			"phases Pending/ImportScheduled stuck for long usually mean no matching StorageProfile or no capacity")
	}
	// A finished DataVolume routinely sits at Running=False with no reason, and
	// Bound=False is normal for WaitForFirstConsumer storage. Only surface these
	// while the import is still meant to be progressing.
	if phase != "Succeeded" {
		if msg := conditionMessage(dv.Object, "Running", "False"); msg != "" {
			node.Note("condition Running=False: " + msg)
		}
		if msg := conditionMessage(dv.Object, "Bound", "False"); msg != "" {
			node.Note("condition Bound=False: " + msg)
		}
	}

	r.describeDataVolumeOrigin(ctx, ns, dv, node, depth)

	claim := nstr(dv.Object, "status", "claimName")
	if claim == "" {
		claim = dv.GetName() // CDI names the PVC after the DataVolume
	}
	r.describePVC(ctx, ns, claim, node, depth, pvcContext{dvSource: declaredDVSource(dv)})
}

// declaredDVSource renders, in one line, the source a DataVolume declares in its
// spec. This is what the DataVolume was templated with, which is not always what
// ended up populating the PVC.
func declaredDVSource(dv *unstructured.Unstructured) string {
	src := nmap(dv.Object, "spec", "source")
	if p := asMap(src["pvc"]); p != nil {
		return "PVC " + joinNonEmpty("/", nstr(p, "namespace"), nstr(p, "name"))
	}
	if s := asMap(src["snapshot"]); s != nil {
		return "VolumeSnapshot " + joinNonEmpty("/", nstr(s, "namespace"), nstr(s, "name"))
	}
	for _, k := range []string{"http", "s3", "gcs", "registry", "upload", "blank", "imageio", "vddk"} {
		if src[k] != nil {
			return k + " import"
		}
	}
	if k := nstr(dv.Object, "spec", "sourceRef", "kind"); k != "" {
		return k + " " + nstr(dv.Object, "spec", "sourceRef", "name")
	}
	return ""
}

// describeDataVolumeOrigin walks upward from a DataVolume: spec.source tells you
// where the bits came from, and spec.sourceRef points at a DataSource which is
// itself usually managed by a DataImportCron. This is the DataVolume-side
// analogue of "what created the snapshot".
func (r *Resolver) describeDataVolumeOrigin(ctx context.Context, ns string, dv *unstructured.Unstructured, node *tree.Node, depth int) {
	if depth >= maxProvenanceDepth {
		return
	}

	// spec.sourceRef -> DataSource
	if kind := nstr(dv.Object, "spec", "sourceRef", "kind"); kind != "" {
		dsName := nstr(dv.Object, "spec", "sourceRef", "name")
		dsNS := nstr(dv.Object, "spec", "sourceRef", "namespace")
		if dsNS == "" {
			dsNS = ns
		}
		n := node.Add(tree.New(fmt.Sprintf("⤴ sourceRef %s/%s", kind, dsName)).SetState(tree.StateInfo))
		n.Attr("namespace", dsNS)
		ds, err := r.src.Get(ctx, source.Ref{Kind: kind, Namespace: dsNS, Name: dsName})
		if err != nil {
			n.Escalate(tree.StateMissing)
			r.rep.Problem(tree.StateMissing, "DataVolume/"+dv.GetName(),
				fmt.Sprintf("sourceRef %s %q in namespace %q does not exist", kind, dsName, dsNS),
				"a DataVolume whose DataSource is missing never starts importing; for golden images check the openshift-virtualization-os-images namespace")
		} else {
			if msg := conditionMessage(ds.Object, "Ready", "False"); msg != "" {
				n.Escalate(tree.StateWarn)
				n.Note("DataSource Ready=False: " + msg)
			}
			// status.source is the resolved source, spec.source the configured one.
			r.describeDataSourceSource(ctx, dsNS, ds, n, depth+1)
			r.addManagingCron(ctx, dsNS, ds.GetName(), n)
		}
	}

	// spec.source.* -> concrete origin
	src := nmap(dv.Object, "spec", "source")
	for key, val := range src {
		m := asMap(val)
		switch key {
		case "pvc":
			srcNS := nstr(m, "namespace")
			if srcNS == "" {
				srcNS = ns
			}
			n := node.Add(tree.New("⤴ cloned from PVC/"+nstr(m, "name")).SetState(tree.StateInfo)).Attr("namespace", srcNS)
			r.describePVCShallow(ctx, srcNS, nstr(m, "name"), n)
		case "snapshot":
			srcNS := nstr(m, "namespace")
			if srcNS == "" {
				srcNS = ns
			}
			vsName := nstr(m, "name")
			n := node.Add(tree.New("⤴ restored from VolumeSnapshot/"+vsName).SetState(tree.StateInfo)).Attr("namespace", srcNS)
			vs, err := r.src.Get(ctx, source.Ref{Kind: "VolumeSnapshot", Namespace: srcNS, Name: vsName})
			switch {
			case err == nil:
				if depth+1 < maxProvenanceDepth {
					r.describeVolumeSnapshot(ctx, vs, n, "")
				}
			case nstr(dv.Object, "status", "phase") == "Succeeded":
				// The import already finished; a rotated source snapshot is normal.
				n.Note("source snapshot no longer exists (expected after a completed import)")
			default:
				n.Escalate(tree.StateMissing)
				r.rep.Problem(tree.StateMissing, "DataVolume/"+dv.GetName(),
					fmt.Sprintf("DataVolume is %q and its source VolumeSnapshot %q in namespace %q does not exist",
						nstr(dv.Object, "status", "phase"), vsName, srcNS),
					"the import cannot proceed without its source snapshot")
			}
		case "http", "s3", "gcs", "registry", "upload", "blank", "imageio", "vddk":
			n := node.Add(tree.New("⤴ imported from " + key).SetState(tree.StateInfo))
			n.Attr("url", joinNonEmpty(" ", nstr(m, "url"), nstr(m, "secretRef")))
		}
	}
}

// describeDataSourceSource follows a DataSource to the PVC or VolumeSnapshot it
// resolves to, preferring status (resolved) over spec (configured).
func (r *Resolver) describeDataSourceSource(ctx context.Context, ns string, ds *unstructured.Unstructured, node *tree.Node, depth int) {
	if depth >= maxProvenanceDepth {
		return
	}
	for _, root := range []string{"status", "spec"} {
		src := nmap(ds.Object, root, "source")
		if len(src) == 0 {
			continue
		}
		if pvc := asMap(src["pvc"]); pvc != nil {
			pns := nstr(pvc, "namespace")
			if pns == "" {
				pns = ns
			}
			n := node.Add(tree.New("→ PVC/"+nstr(pvc, "name")).SetState(tree.StateInfo)).Attr("namespace", pns)
			r.describePVCShallow(ctx, pns, nstr(pvc, "name"), n)
		}
		if snap := asMap(src["snapshot"]); snap != nil {
			sns := nstr(snap, "namespace")
			if sns == "" {
				sns = ns
			}
			vsName := nstr(snap, "name")
			n := node.Add(tree.New("→ VolumeSnapshot/"+vsName).SetState(tree.StateInfo)).Attr("namespace", sns)
			vs, err := r.src.Get(ctx, source.Ref{Kind: "VolumeSnapshot", Namespace: sns, Name: vsName})
			if err != nil {
				n.Escalate(tree.StateMissing)
				r.rep.Problem(tree.StateMissing, "DataSource/"+ds.GetName(),
					fmt.Sprintf("DataSource resolves to VolumeSnapshot %q in %q which does not exist", vsName, sns),
					"every DataVolume using this DataSource will fail to start")
			} else {
				r.describeVolumeSnapshot(ctx, vs, n, "")
			}
		}
		return // status wins; do not also render spec
	}
}

// addManagingCron finds the DataImportCron that manages a DataSource.
func (r *Resolver) addManagingCron(ctx context.Context, ns, dsName string, node *tree.Node) {
	for _, cron := range r.listKind(ctx, "DataImportCron", ns) {
		if nstr(cron.Object, "spec", "managedDataSource") != dsName {
			continue
		}
		n := node.Add(tree.New("⤴ DataImportCron/" + cron.GetName()).SetState(tree.StateInfo))
		n.Attr("schedule", nstr(cron.Object, "spec", "schedule"))
		n.Attr("lastImport", nstr(cron.Object, "status", "lastImportTimestamp"))
		n.Attr("sourceFormat", nstr(cron.Object, "status", "sourceFormat"))
		if p := nstr(cron.Object, "status", "lastImportedPVC", "name"); p != "" {
			n.Note("lastImportedPVC: " + p)
		}
		if msg := conditionMessage(cron.Object, "UpToDate", "False"); msg != "" {
			n.Escalate(tree.StateWarn)
			n.Note("UpToDate=False: " + msg)
		}
	}
}

// describePVCShallow reports whether a referenced PVC exists, without descending
// into the PV and array layers. Absence is flagged but not escalated: this is
// used for import provenance, where the source often rotates away legitimately.
func (r *Resolver) describePVCShallow(ctx context.Context, ns, name string, node *tree.Node) {
	pvc, err := r.src.Get(ctx, source.Ref{Kind: "PersistentVolumeClaim", Namespace: ns, Name: name})
	if err != nil {
		node.Escalate(tree.StateWarn)
		node.Note("PVC does not exist")
		return
	}
	node.Attr("phase", nstr(pvc.Object, "status", "phase"))
	node.Attr("size", nqty(pvc.Object, "status", "capacity", "storage"))
	node.Attr("sc", nstr(pvc.Object, "spec", "storageClassName"))
}

// pvcContext carries what the caller already knows about a PVC, so the report can
// withhold generic advice that does not apply to this particular one.
type pvcContext struct {
	// dvSource is the source declared by the owning DataVolume, if any, letting a
	// disagreement with the PVC's own dataSourceRef be pointed out.
	dvSource string
	// stray marks a PVC created by a restore that never completed. Nothing will
	// ever consume it, so the usual WaitForFirstConsumer explanation for Pending is
	// actively wrong, and the caller has already reported its phase.
	stray bool
}

// describePVC resolves a PVC downward to the array.
func (r *Resolver) describePVC(ctx context.Context, ns, name string, parent *tree.Node, depth int, pctx pvcContext) {
	pvc, node := r.getOrNode(ctx, source.Ref{Kind: "PersistentVolumeClaim", Namespace: ns, Name: name}, "PVC lookup")
	parent.Add(node)
	if pvc == nil {
		return
	}
	where := "PersistentVolumeClaim/" + name

	phase := nstr(pvc.Object, "status", "phase")
	node.Attr("phase", phase)
	node.Attr("size", joinNonEmpty("/",
		nqty(pvc.Object, "status", "capacity", "storage"),
		nqty(pvc.Object, "spec", "resources", "requests", "storage")))
	node.Attr("sc", nstr(pvc.Object, "spec", "storageClassName"))
	node.Attr("volumeMode", nstr(pvc.Object, "spec", "volumeMode"))
	node.Attr("accessModes", strings.Join(stringSlice(nslice(pvc.Object, "spec", "accessModes")), ","))

	// Resolve the class first: its binding mode decides whether Pending is normal.
	bindingMode := r.describeStorageClass(ctx, nstr(pvc.Object, "spec", "storageClassName"), node)

	if phase != "Bound" {
		node.Escalate(tree.StateWarn)
		if pctx.stray {
			// Do not offer the WaitForFirstConsumer explanation: this PVC belongs to
			// an abandoned restore, so no consumer is ever coming.
			node.Note("will never bind: nothing will consume a PVC left by an incomplete restore")
		} else {
			r.rep.Problem(tree.StateWarn, where,
				fmt.Sprintf("PVC phase is %q, not Bound", phase),
				pendingHint(bindingMode))
		}
	}
	if pvc.GetDeletionTimestamp() != nil {
		node.Escalate(tree.StateWarn)
		node.Note("terminating since " + pvc.GetDeletionTimestamp().String())
		if f := pvc.GetFinalizers(); len(f) > 0 {
			node.Note("finalizers blocking deletion: " + strings.Join(f, ", "))
		}
		r.rep.Problem(tree.StateWarn, where, "PVC is stuck terminating",
			"a finalizer (usually kubernetes.io/pvc-protection) is held by a running pod or VMI still using it")
	}
	r.rep.At(createdAt(pvc), "PVC "+name+" created", tree.StateInfo)

	// Who else holds this volume -- the usual answer to "why will it not attach".
	r.addPVCConsumers(ctx, pvc, node)

	// Provenance: a PVC restored from a snapshot records it here.
	r.describePVCDataSource(ctx, ns, pvc, node, depth, pctx.dvSource)

	pvName := nstr(pvc.Object, "spec", "volumeName")
	if pvName == "" {
		node.Note("no spec.volumeName: never bound to a PersistentVolume")
		return
	}
	r.describePV(ctx, pvName, node, pvc)
}

// describePVCDataSource shows what a PVC was populated from. The reference is
// reported at any depth -- whether a restored PVC is even wired to its snapshot is
// exactly what you need to know -- while only the recursive dive is bounded.
func (r *Resolver) describePVCDataSource(ctx context.Context, ns string, pvc *unstructured.Unstructured, node *tree.Node, depth int, dvSource string) {
	for _, path := range []string{"dataSourceRef", "dataSource"} {
		ref := nmap(pvc.Object, "spec", path)
		if len(ref) == 0 {
			continue
		}
		kind := nstr(ref, "kind")
		name := nstr(ref, "name")
		if kind == "" || name == "" {
			continue
		}
		refNS := nstr(ref, "namespace")
		if refNS == "" {
			refNS = ns
		}
		n := node.Add(tree.New(fmt.Sprintf("⤴ spec.%s %s/%s", path, kind, name)).SetState(tree.StateInfo))

		// CDI populators are a level of indirection, not a different origin: a
		// VolumeCloneSource wraps the very PVC the DataVolume declares. Resolve it
		// so the comparison below is against the real source, and so the chain does
		// not dead-end on a wrapper object.
		effKind, effName, effNS := kind, name, refNS
		if k2, n2, ns2, ok := r.resolvePopulator(ctx, refNS, kind, name, n); ok {
			effKind, effName, effNS = k2, n2, ns2
		}

		// A restored PVC keeps the DataVolume spec it was templated from while
		// actually being populated from a snapshot. Saying which one delivered the
		// data avoids chasing the wrong volume on the array -- but only when they
		// genuinely differ, which is why the populator is resolved first.
		if dvSource != "" && effName != "" && !strings.Contains(dvSource, effName) {
			n.Note("this is what populated the volume; the DataVolume separately declares its source as " + dvSource)
		}

		if effKind == "VolumeSnapshot" {
			kind, name, refNS = effKind, effName, effNS
		}
		if kind == "VolumeSnapshot" {
			vs, err := r.src.Get(ctx, source.Ref{Kind: "VolumeSnapshot", Namespace: refNS, Name: name})
			switch {
			case err == nil:
				// Always resolve the snapshot behind a populated PVC. Repetition is
				// handled by TrimRepeats inside describeVolumeSnapshot, not by
				// silently stopping here.
				r.describeVolumeSnapshot(ctx, vs, n, "")
			case nstr(pvc.Object, "status", "phase") == "Bound":
				// Populated and bound: the origin snapshot has served its purpose
				// and golden-image snapshots rotate. Lineage only, not a fault.
				n.Note("origin snapshot no longer exists (expected once the PVC is populated)")
			default:
				n.Escalate(tree.StateMissing)
				n.Note("the snapshot this PVC is being populated from is gone")
				r.rep.Problem(tree.StateMissing, "PersistentVolumeClaim/"+pvc.GetName(),
					fmt.Sprintf("PVC is %q and its source VolumeSnapshot %q is missing",
						nstr(pvc.Object, "status", "phase"), name),
					"population can never complete; recreate the source snapshot or the PVC")
			}
		}
		return // dataSourceRef wins over the deprecated dataSource
	}
}

func (r *Resolver) describePV(ctx context.Context, pvName string, parent *tree.Node, pvc *unstructured.Unstructured) {
	pv, node := r.getOrNode(ctx, source.Ref{Kind: "PersistentVolume", Name: pvName}, "PV lookup")
	parent.Add(node)
	if pv == nil {
		return
	}
	where := "PersistentVolume/" + pvName

	node.Attr("phase", nstr(pv.Object, "status", "phase"))
	node.Attr("reclaimPolicy", nstr(pv.Object, "spec", "persistentVolumeReclaimPolicy"))
	if msg := nstr(pv.Object, "status", "message"); msg != "" {
		node.Note("status.message: " + msg)
	}
	node.Attr("capacity", nqty(pv.Object, "spec", "capacity", "storage"))

	// Confirm the PV points back at this PVC. A crossed claimRef is how two VMs
	// end up fighting over one volume.
	crName := nstr(pv.Object, "spec", "claimRef", "name")
	crNS := nstr(pv.Object, "spec", "claimRef", "namespace")
	if crName != "" && (crName != pvc.GetName() || crNS != pvc.GetNamespace()) {
		node.Escalate(tree.StateError)
		r.rep.Problem(tree.StateError, where,
			fmt.Sprintf("PV claimRef is %s/%s but it is bound by %s/%s", crNS, crName, pvc.GetNamespace(), pvc.GetName()),
			"crossed claimRef; the binding is inconsistent and needs manual correction")
	}

	r.addVolumeAttachments(ctx, pvName, node)
	r.checkCapacityConsistency(
		nqty(pvc.Object, "spec", "resources", "requests", "storage"),
		nqty(pv.Object, "spec", "capacity", "storage"),
		"", "PersistentVolumeClaim/"+pvc.GetName(), node)

	csi := nmap(pv.Object, "spec", "csi")
	if len(csi) == 0 {
		node.Note("not a CSI volume (" + nonCSIKind(pv.Object) + "); no array handle to resolve")
		return
	}
	driver := nstr(csi, "driver")
	handle := nstr(csi, "volumeHandle")
	node.Attr("driver", driver)
	node.Attr("volumeHandle", handle)
	// volumeAttributes frequently carry the array-side names verbatim.
	for _, k := range []string{"backend", "pod", "array", "volumeName", "serial"} {
		if v := fieldStr(csi, "volumeAttributes", k); v != "" {
			node.Note("volumeAttributes." + k + "=" + v)
		}
	}

	if handle == "" {
		node.Escalate(tree.StateMissing)
		r.rep.Problem(tree.StateMissing, where, "CSI volumeHandle is empty",
			"nothing identifies this volume on the array")
		return
	}
	if r.opt.SkipArray {
		return
	}
	r.addArrayObject(ctx, node, handle, arrayLookup{
		kind: r.opt.ArrayVolumeKind,
		spec: arrayVolumeFields,
	})
}

func nonCSIKind(o map[string]interface{}) string {
	for _, k := range []string{"nfs", "iscsi", "fc", "hostPath", "local", "rbd", "cephfs"} {
		if len(nmap(o, "spec", k)) > 0 {
			return k
		}
	}
	return "unknown backend"
}

// resolvePopulator follows a CDI volume-populator object to the thing it actually
// clones from.
//
// CDI puts a VolumeCloneSource (or VolumeImportSource / VolumeUploadSource)
// between a PVC and its real source. Reporting the wrapper alone makes the chain
// dead-end on an object whose name carries no information, and makes a populated
// PVC look as though its provenance disagrees with its DataVolume when the two are
// in fact the same thing.
func (r *Resolver) resolvePopulator(ctx context.Context, ns, kind, name string, node *tree.Node) (string, string, string, bool) {
	switch kind {
	case "VolumeCloneSource", "VolumeImportSource", "VolumeUploadSource":
	default:
		return "", "", "", false
	}

	obj, err := r.src.Get(ctx, source.Ref{Kind: kind, Namespace: ns, Name: name})
	if err != nil {
		node.Note(kind + " does not exist; CDI deletes a populator once the target PVC is populated")
		return "", "", "", false
	}

	// VolumeCloneSource.spec.source is a TypedLocalObjectReference to a PVC or a
	// VolumeSnapshot in the same namespace.
	srcKind := nstr(obj.Object, "spec", "source", "kind")
	srcName := nstr(obj.Object, "spec", "source", "name")
	if srcKind != "" && srcName != "" {
		node.Note(fmt.Sprintf("populator wrapping %s/%s", srcKind, srcName))
		return srcKind, srcName, ns, true
	}

	// VolumeImportSource carries an import type rather than an object reference.
	if imp := nmap(obj.Object, "spec", "source"); len(imp) > 0 {
		kinds := make([]string, 0, len(imp))
		for k := range imp {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		node.Note("populator importing from " + strings.Join(kinds, ","))
	}
	return "", "", "", false
}

// pendingHint explains a Pending PVC using the binding mode we actually read,
// rather than offering both possibilities and letting the reader pick wrong.
func pendingHint(bindingMode string) string {
	switch bindingMode {
	case "WaitForFirstConsumer":
		return "bindingMode is WaitForFirstConsumer, so Pending is expected until a pod consumes it; if no pod ever will, this PVC is simply unused"
	case "Immediate":
		return "bindingMode is Immediate, so Pending means provisioning itself is failing -- read the events on this PVC and the CSI provisioner logs"
	}
	return "check the events on this PVC and the provisioner logs"
}

// conditionTime returns a condition's lastTransitionTime, which is when a failure
// actually happened rather than when its object was created.
func conditionTime(o map[string]interface{}, condType, condStatus string) string {
	for _, raw := range nslice(o, "status", "conditions") {
		m := asMap(raw)
		if nstr(m, "type") == condType && nstr(m, "status") == condStatus {
			return nstr(m, "lastTransitionTime")
		}
	}
	return ""
}

// conditionMessage returns the reason/message of a status condition matching
// type and status, used by CDI and restore objects which report failure detail
// only there. A matching condition that carries no text still returns a
// non-empty string, so callers never silently drop it.
func conditionMessage(o map[string]interface{}, condType, condStatus string) string {
	for _, raw := range nslice(o, "status", "conditions") {
		m := asMap(raw)
		if nstr(m, "type") != condType || nstr(m, "status") != condStatus {
			continue
		}
		if text := joinNonEmpty(": ", nstr(m, "reason"), nstr(m, "message")); text != "" {
			return text
		}
		return "(no reason given)"
	}
	return ""
}
