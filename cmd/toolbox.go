package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mjovanovic0/kubectl-toolbox/internal/selfupdate"
	"github.com/mjovanovic0/kubectl-toolbox/internal/version"
)

// newVMSnapshotCommand groups the KubeVirt storage resolver. Every tool in the
// toolbox lives under its own noun so the binary can grow without its top-level
// help turning into a flat list of unrelated verbs.
func newVMSnapshotCommand(g *globals) *cobra.Command {
	c := &cobra.Command{
		Use:     "vmsnapshot",
		Aliases: []string{"vmsnap", "vmstorage"},
		Short:   "Resolve KubeVirt VM snapshot and disk chains down to the storage array",
		Long: `Resolve the full reference chain behind an OpenShift Virtualization VM's
snapshots and disks, so you can find the object to inspect on the storage array.

Snapshot chain:
  VirtualMachine -> VirtualMachineSnapshot -> VirtualMachineSnapshotContent
    -> VolumeSnapshot -> VolumeSnapshotContent -> PureSnapshot

Disk chain:
  DataImportCron -> DataSource -> DataVolume -> PersistentVolumeClaim
    -> PersistentVolume -> PureVolume

Every reference is verified, and broken ones are reported with the reason they
usually break. Exits non-zero when a reference is missing or errored.`,
	}
	c.AddCommand(
		newVMCommand(g),
		newListCommand(g),
		newSnapshotCommand(g),
		newDisksCommand(g),
		newPVCCommand(g),
		newVolumeSnapshotCommand(g),
	)
	return c
}

func newVersionCommand(g *globals) *cobra.Command {
	var check bool
	c := &cobra.Command{
		Use:   "version",
		Short: "Print the build version, and optionally check for a newer release",
		RunE: func(cmd *cobra.Command, args []string) error {
			i := version.Current()
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(w, "version\t%s\n", i.Version)
			if i.Commit != "" {
				fmt.Fprintf(w, "commit\t%s\n", i.Commit)
			}
			if i.BuildDate != "" {
				fmt.Fprintf(w, "built\t%s\n", i.BuildDate)
			}
			fmt.Fprintf(w, "go\t%s\n", i.GoVersion)
			fmt.Fprintf(w, "platform\t%s\n", i.Platform)
			if t, err := selfupdate.Locate(); err == nil {
				fmt.Fprintf(w, "installed at\t%s (writable=%t)\n", t.Path, t.Writable)
			}
			if !version.IsRelease() {
				fmt.Fprintf(w, "build type\tlocal build, so update checks are skipped\n")
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if !check {
				return nil
			}

			// An explicit check bypasses the cache: the user asked right now.
			rel, newer, err := selfupdate.New().Available(cmd.Context(), true)
			switch {
			case err != nil:
				fmt.Fprintf(os.Stderr, "\nupdate check failed: %v\n", err)
			case !version.IsRelease():
				fmt.Println("\nthis is a local build; nothing to compare against")
			case newer:
				fmt.Printf("\n%s is available (%s)\n", rel.TagName, rel.HTMLURL)
			default:
				fmt.Println("\nup to date")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&check, "check", false, "query GitHub for a newer release, ignoring the cache")
	return c
}

func newUpdateCommand(g *globals) *cobra.Command {
	var assumeYes bool
	c := &cobra.Command{
		Use:   "update",
		Short: "Replace this binary with the newest release",
		Long: `Check GitHub for a newer release and, if there is one, download it and replace
this binary in place.

The download is verified against the checksums file published with the release
before anything is installed, and the replacement is atomic: the new binary is
staged alongside the old one and moved into place, so an interrupted update cannot
leave a broken executable behind.

On a host with no route to GitHub -- which is where this tool is often used -- the
check fails fast and reports what to do instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			u := selfupdate.New()

			if !version.IsRelease() {
				fmt.Printf("This is a local build (%s), not an installed release.\n", version.Version)
				fmt.Println("Updating would replace it with a published binary. Re-run with --yes to do that anyway.")
				if !assumeYes {
					return nil
				}
			}

			target, err := selfupdate.Locate()
			if err != nil {
				return err
			}

			// Always bypass the cache here: the user asked to update now.
			rel, err := u.Latest(cmd.Context(), true)
			if err != nil {
				return fmt.Errorf("%w\n\nIf this host has no route to GitHub, download the release elsewhere and "+
					"copy it across, or use the self-extracting text installer from the release page", err)
			}
			if version.IsRelease() {
				newer, err := version.Newer(version.Version, rel.TagName)
				if err != nil {
					return err
				}
				if !newer {
					fmt.Printf("Already up to date (%s).\n", version.Version)
					return nil
				}
			}

			fmt.Printf("Current:   %s\n", version.Version)
			fmt.Printf("Latest:    %s  (%s)\n", rel.TagName, rel.HTMLURL)
			fmt.Printf("Replacing: %s\n", target.Path)
			fmt.Printf("Asset:     %s\n", selfupdate.AssetName(rel.TagName))

			if !assumeYes {
				ok, err := confirm(cmd.Context(), "Download and replace this binary?")
				if err != nil {
					return err
				}
				if !ok {
					fmt.Println("Cancelled.")
					return nil
				}
			}

			fmt.Println("Downloading and verifying ...")
			if err := u.Apply(cmd.Context(), rel, target); err != nil {
				return err
			}
			fmt.Printf("Updated to %s.\n", rel.TagName)
			return nil
		},
	}
	c.Flags().BoolVarP(&assumeYes, "yes", "y", false, "do not ask for confirmation")
	return c
}

// confirm asks on the terminal. A non-interactive stdin is treated as "no" so an
// update never happens silently in a script.
func confirm(ctx context.Context, prompt string) (bool, error) {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		fmt.Println("stdin is not a terminal; re-run with --yes to confirm non-interactively")
		return false, nil
	}
	fmt.Printf("%s [y/N] ", prompt)
	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		return false, nil
	}
	switch answer {
	case "y", "Y", "yes", "Yes":
		return true, nil
	}
	return false, nil
}
