package resolve

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/mjovanovic0/kubectl-vmstorage/internal/source"
	"github.com/mjovanovic0/kubectl-vmstorage/internal/tree"
)

// storageClass fetches and memoises a StorageClass.
func (r *Resolver) storageClass(ctx context.Context, name string) *unstructured.Unstructured {
	if name == "" {
		return nil
	}
	if sc, ok := r.classCache[name]; ok {
		return sc
	}
	sc, err := r.src.Get(ctx, source.Ref{Kind: "StorageClass", Name: name})
	if err != nil {
		sc = nil
	}
	r.classCache[name] = sc
	return sc
}

// volumeSnapshotClasses lists the snapshot classes once per run.
func (r *Resolver) volumeSnapshotClasses(ctx context.Context) []*unstructured.Unstructured {
	if r.snapClassesLoaded {
		return r.snapClasses
	}
	r.snapClassesLoaded = true
	r.snapClasses = r.listKind(ctx, "VolumeSnapshotClass", "")
	return r.snapClasses
}

// describeStorageClass annotates a PVC node with the facts its storage class
// determines, replacing guesses with answers:
//
//   - volumeBindingMode explains a Pending PVC outright (WaitForFirstConsumer is
//     supposed to stay Pending until a consumer appears).
//   - whether any VolumeSnapshotClass exists for this provisioner decides whether
//     the disk can be snapshotted at all, which is the usual reason a VM snapshot
//     silently skips a volume.
//
// It returns the effective volumeBindingMode so callers can explain a Pending
// PVC accurately instead of guessing.
func (r *Resolver) describeStorageClass(ctx context.Context, scName string, node *tree.Node) string {
	if scName == "" {
		node.Note("no storageClassName: statically provisioned, or the cluster default applied")
		return ""
	}
	sc := r.storageClass(ctx, scName)
	if sc == nil {
		node.Escalate(tree.StateWarn)
		node.Note("StorageClass " + scName + " does not exist")
		r.rep.Problem(tree.StateWarn, "StorageClass/"+scName,
			fmt.Sprintf("storage class %q referenced by a PVC does not exist", scName),
			"a PVC naming a missing class can never be provisioned; the class may have been deleted after the PVC was created")
		return ""
	}

	provisioner := nstr(sc.Object, "provisioner")
	binding := nstr(sc.Object, "volumeBindingMode")
	if binding == "" {
		binding = "Immediate" // the API default when unset
	}
	node.Note(fmt.Sprintf("storageClass %s: provisioner=%s bindingMode=%s reclaim=%s expansion=%s",
		scName, provisioner, binding,
		orDash(nstr(sc.Object, "reclaimPolicy")),
		orDash(fieldStr(sc.Object, "allowVolumeExpansion"))))
	r.checkSnapshotSupport(ctx, scName, provisioner, node)
	return binding
}

// checkSnapshotSupport reports whether this provisioner has a VolumeSnapshotClass,
// which is what decides if a disk is snapshottable.
func (r *Resolver) checkSnapshotSupport(ctx context.Context, scName, provisioner string, node *tree.Node) {
	if provisioner == "" {
		return
	}
	classes := r.volumeSnapshotClasses(ctx)
	if classes == nil {
		// Cannot tell -- do not imply either answer.
		node.Note("could not list VolumeSnapshotClasses; snapshot support for " + provisioner + " is unverified")
		return
	}
	var matching []string
	for _, c := range classes {
		if nstr(c.Object, "driver") != provisioner {
			continue
		}
		name := c.GetName()
		if isDefaultSnapshotClass(c) {
			name += " (default)"
		}
		matching = append(matching, name+" deletionPolicy="+orDash(nstr(c.Object, "deletionPolicy")))
	}
	if len(matching) == 0 {
		node.Escalate(tree.StateWarn)
		node.Note("NO VolumeSnapshotClass for driver " + provisioner + ": this disk cannot be snapshotted")
		r.rep.Problem(tree.StateWarn, "StorageClass/"+scName,
			fmt.Sprintf("no VolumeSnapshotClass exists for driver %q", provisioner),
			"KubeVirt silently excludes such volumes from a VirtualMachineSnapshot -- create a VolumeSnapshotClass for this driver, or accept that this disk is never captured")
		return
	}
	node.Note("snapshottable via VolumeSnapshotClass " + strings.Join(matching, ", "))
}

func isDefaultSnapshotClass(c *unstructured.Unstructured) bool {
	for k, v := range c.GetAnnotations() {
		if strings.HasSuffix(k, "is-default-class") && v == "true" {
			return true
		}
	}
	return false
}

// explainUnsnapshottedDisk turns the "probably no VolumeSnapshotClass" guess into
// a verified statement, for a volumeBackup entry that captured no snapshot.
func (r *Resolver) explainUnsnapshottedDisk(ctx context.Context, scName string, node *tree.Node) {
	if scName == "" {
		return
	}
	sc := r.storageClass(ctx, scName)
	if sc == nil {
		node.Note("storage class " + scName + " no longer exists, so snapshot support cannot be confirmed")
		return
	}
	provisioner := nstr(sc.Object, "provisioner")
	for _, c := range r.volumeSnapshotClasses(ctx) {
		if nstr(c.Object, "driver") == provisioner {
			node.Note(fmt.Sprintf("CONFIRMED NOT the cause: driver %s does have VolumeSnapshotClass %s, so look elsewhere",
				provisioner, c.GetName()))
			return
		}
	}
	node.Note(fmt.Sprintf("CONFIRMED cause: storage class %s uses driver %s, which has no VolumeSnapshotClass",
		scName, provisioner))
}
