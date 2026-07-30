package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mjovanovic0/kubectl-vmstorage/internal/resolve"
)

func newVMCommand(g *globals) *cobra.Command {
	var vopt struct {
		noDisks     bool
		noSnapshots bool
		latest      int
		noRuntime   bool
	}
	c := &cobra.Command{
		Use:   "vm <vm-name>",
		Short: "Resolve everything about one VM: live disks and all its snapshots",
		Long: `Resolve both chains for a VM in a single pass -- this is the usual entry point.

  disks      every volume -> DataVolume -> PVC -> PV -> PureVolume
  snapshots  every VirtualMachineSnapshot of this VM -> content -> volumeBackups
             -> VolumeSnapshot -> VolumeSnapshotContent -> PureSnapshot

Use --latest to resolve only the most recent snapshots on a VM with a long
snapshot history, or --no-disks / --no-snapshots to look at one half.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := g.resolver()
			if err != nil {
				return err
			}
			return g.emit(r.VM(cmd.Context(), args[0], resolve.VMOptions{
				SkipDisks:     vopt.noDisks,
				SkipSnapshots: vopt.noSnapshots,
				Latest:        vopt.latest,
				SkipRuntime:   vopt.noRuntime,
			}))
		},
	}
	c.Flags().BoolVar(&vopt.noDisks, "no-disks", false, "skip the live disk chain")
	c.Flags().BoolVar(&vopt.noSnapshots, "no-snapshots", false, "skip the snapshot chains")
	c.Flags().IntVar(&vopt.latest, "latest", 0, "resolve only the N most recent snapshots (0 = all)")
	c.Flags().BoolVar(&vopt.noRuntime, "no-runtime", false,
		"skip the live VirtualMachineInstance / virt-launcher view")
	return c
}

func newListCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list [vm]",
		Short: "List VirtualMachineSnapshots, optionally for one VM",
		Long: `List VirtualMachineSnapshots in the namespace. Given a VM name, only snapshots
whose spec.source points at that VM are shown. The ISSUES column flags snapshots
whose content is missing or whose disks were never snapshotted.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := g.resolver()
			if err != nil {
				return err
			}
			vm := ""
			if len(args) == 1 {
				vm = args[0]
			}
			rows, err := r.ListSnapshots(cmd.Context(), vm)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				if vm != "" {
					fmt.Fprintf(os.Stderr, "no VirtualMachineSnapshots found for VM %q\n", vm)
				} else {
					fmt.Fprintln(os.Stderr, "no VirtualMachineSnapshots found")
				}
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tVM\tPHASE\tREADY\tVOLUMES\tCREATED\tCONTENT\tISSUES")
			for _, row := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					row.Name, row.VM, row.Phase, row.Ready, row.Volumes, row.Created, row.Content, row.Issues)
			}
			return w.Flush()
		},
	}
}

func newSnapshotCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "snapshot <vm> <snapshot>",
		Aliases: []string{"tree", "resolve"},
		Short:   "Resolve a VM snapshot down to the storage array",
		Long: `Resolve the whole reference chain of one VirtualMachineSnapshot:

  VirtualMachine
    VirtualMachineSnapshot        status.virtualMachineSnapshotContentName
    VirtualMachineSnapshotContent spec.volumeBackups[] (disk, source PVC spec, snapshot name)
    VolumeSnapshot                status.boundVolumeSnapshotContentName
    VolumeSnapshotContent         status.snapshotHandle
    PureSnapshot                  array pod, backends, targets

Each volumeBackup entry also reports whether its original PVC still exists, which
is the fastest way to tell a deleted PVC apart from a snapshot that never
captured it.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := g.resolver()
			if err != nil {
				return err
			}
			return g.emit(r.Snapshot(cmd.Context(), args[0], args[1]))
		},
	}
}

func newDisksCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "disks <vm>",
		Aliases: []string{"volumes"},
		Short:   "Resolve a VM's live disks down to the storage array",
		Long: `Resolve every volume of a VM through the DataVolume/PVC path:

  VirtualMachine spec.template.spec.volumes[]
    DataVolume              spec.sourceRef -> DataSource <- DataImportCron
    PersistentVolumeClaim   spec.volumeName
    PersistentVolume        spec.csi.volumeHandle
    PureVolume              array serial, pod, backends

Import provenance is walked upward too, so a disk restored from a VolumeSnapshot
or cloned from another PVC shows where it came from.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := g.resolver()
			if err != nil {
				return err
			}
			return g.emit(r.Disks(cmd.Context(), args[0]))
		},
	}
}

func newPVCCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "pvc <name>",
		Short: "Resolve a single PVC down to the array and back to its origin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := g.resolver()
			if err != nil {
				return err
			}
			return g.emit(r.PVCChain(cmd.Context(), args[0]))
		},
	}
}

func newVolumeSnapshotCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "volumesnapshot <name>",
		Aliases: []string{"vs"},
		Short:   "Resolve a single VolumeSnapshot to the array, and find the VM snapshot owning it",
		Long: `Start from a CSI VolumeSnapshot name -- the name that appears in CSI and
storage-operator logs -- and resolve both downward to the array object and upward
to the VirtualMachineSnapshotContent that created it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := g.resolver()
			if err != nil {
				return err
			}
			return g.emit(r.VolumeSnapshotChain(cmd.Context(), args[0]))
		},
	}
}
