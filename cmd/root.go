// Package cmd wires the CLI.
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/mjovanovic0/kubectl-vmsnap/internal/resolve"
	"github.com/mjovanovic0/kubectl-vmsnap/internal/source"
	"github.com/mjovanovic0/kubectl-vmsnap/internal/tree"
)

// Version is overridden at build time with -ldflags.
var Version = "dev"

type globals struct {
	configFlags *genericclioptions.ConfigFlags

	dump              string
	noColor           bool
	noArray           bool
	noEvents          bool
	allEvents         bool
	timeline          bool
	trimRepeats       bool
	showOwners        bool
	arraySnapshotKind string
	arrayVolumeKind   string

	// exitCode is set by emit so Execute can exit non-zero on a broken chain
	// without bypassing cobra's own error handling.
	exitCode int
}

// build returns a source plus the namespace to operate in.
func (g *globals) build() (source.Source, string, error) {
	if g.dump != "" {
		d, err := source.NewDump(g.dump)
		if err != nil {
			return nil, "", err
		}
		ns := ""
		if g.configFlags.Namespace != nil {
			ns = *g.configFlags.Namespace
		}
		return d, ns, nil
	}
	live, err := source.NewLive(g.configFlags)
	if err != nil {
		return nil, "", err
	}
	ns := live.DefaultNamespace()
	if g.configFlags.Namespace != nil && *g.configFlags.Namespace != "" {
		ns = *g.configFlags.Namespace
	}
	return live, ns, nil
}

func (g *globals) resolver() (*resolve.Resolver, error) {
	src, ns, err := g.build()
	if err != nil {
		return nil, err
	}
	return resolve.New(src, resolve.Options{
		Namespace:         ns,
		ArraySnapshotKind: g.arraySnapshotKind,
		ArrayVolumeKind:   g.arrayVolumeKind,
		SkipArray:         g.noArray,
		NoEvents:          g.noEvents,
		AllEvents:         g.allEvents,
		Timeline:          g.timeline,
		TrimRepeats:       g.trimRepeats,
		ShowOwners:        g.showOwners,
	}), nil
}

func (g *globals) palette() tree.Palette {
	if g.noColor || os.Getenv("NO_COLOR") != "" {
		return tree.NewPalette(false)
	}
	st, err := os.Stdout.Stat()
	if err != nil {
		return tree.NewPalette(false)
	}
	return tree.NewPalette(st.Mode()&os.ModeCharDevice != 0)
}

// emit renders a report and records the resulting exit code.
func (g *globals) emit(r *tree.Report) error {
	tree.Render(os.Stdout, r, g.palette())
	switch r.WorstState() {
	case tree.StateMissing, tree.StateError:
		g.exitCode = 1
	}
	return nil
}

// NewRootCommand builds the command tree.
func NewRootCommand() (*cobra.Command, *globals) {
	g := &globals{configFlags: genericclioptions.NewConfigFlags(true)}

	// Use is the binary name rather than "kubectl vmsnap": cobra derives every
	// subcommand's usage line from the first word of Use, so a two-word Use would
	// render all of them as "kubectl <sub>".
	root := &cobra.Command{
		Use:   "kubectl-vmsnap",
		Short: "Resolve KubeVirt VM snapshot and disk chains down to the storage array",
		Long: `Resolve the full reference chain behind an OpenShift Virtualization VM's
snapshots and disks, so you can find the object to inspect on the storage array.

Runs either as a kubectl plugin ("kubectl vmsnap ...") or directly
("kubectl-vmsnap ...").

Snapshot chain:
  VirtualMachine -> VirtualMachineSnapshot -> VirtualMachineSnapshotContent
    -> VolumeSnapshot -> VolumeSnapshotContent -> PureSnapshot

Disk chain:
  DataImportCron -> DataSource -> DataVolume -> PersistentVolumeClaim
    -> PersistentVolume -> PureVolume

Every reference is verified, and broken ones are reported with the reason they
usually break. Exits non-zero when a reference is missing or errored.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}

	g.configFlags.AddFlags(root.PersistentFlags())
	pf := root.PersistentFlags()
	// No backticks in flag usage strings: pflag reads backticked text as the
	// argument placeholder name, so "`oc get -o yaml`" would render as the type.
	pf.StringVar(&g.dump, "dump", "",
		"resolve from a directory of YAML/JSON (must-gather or 'oc get -o yaml' capture) instead of the live cluster")
	pf.BoolVar(&g.noColor, "no-color", false, "disable coloured output")
	pf.BoolVar(&g.noArray, "no-array", false, "stop at the CSI handle, skip the vendor array CR lookup")
	pf.BoolVar(&g.noEvents, "no-events", false,
		"do not read Kubernetes events (skips one list call per namespace)")
	pf.BoolVar(&g.allEvents, "events", false,
		"attach events to every resolved object, not just the ones flagged as unhealthy")
	pf.BoolVar(&g.timeline, "timeline", false,
		"add a chronological view of every dated fact found, to establish what happened in which order")
	pf.BoolVar(&g.trimRepeats, "trim-repeats", false,
		"collapse a sub-chain that was already shown in full earlier in the report, instead of repeating it")
	pf.BoolVar(&g.showOwners, "owners", false,
		"walk ownerReferences upward to reveal what created the snapshot (VirtualMachineClone, backup tooling)")
	pf.StringVar(&g.arraySnapshotKind, "array-snapshot-kind", "PureSnapshot",
		"vendor CR kind holding array-side snapshot detail")
	pf.StringVar(&g.arrayVolumeKind, "array-volume-kind", "PureVolume",
		"vendor CR kind holding array-side volume detail")

	root.AddCommand(
		newVMCommand(g),
		newListCommand(g),
		newSnapshotCommand(g),
		newDisksCommand(g),
		newPVCCommand(g),
		newVolumeSnapshotCommand(g),
	)
	return root, g
}

// Execute runs the CLI. Exit 2 signals the tool itself failed; exit 1 signals a
// broken reference was found, which is a successful diagnosis.
func Execute() {
	root, g := NewRootCommand()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	os.Exit(g.exitCode)
}
