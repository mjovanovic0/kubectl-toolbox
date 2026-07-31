package cmd

import (
	"github.com/spf13/cobra"

	"github.com/mjovanovic0/kubectl-toolbox/internal/nodecheck"
)

// newNodeCheckCommand exposes the scheduling-label checker. It reads through the
// shared Source, so unlike the standalone plugin it also works against an offline
// dump and honours the same kubeconfig flags as the rest of the toolbox.
func newNodeCheckCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "nodecheck <pod>",
		Aliases: []string{"label-check", "labelcheck"},
		Short:   "Report pod scheduling label requirements that no node satisfies",
		Long: `Given a pod, report which of its scheduling label requirements -- nodeSelector and
required nodeAffinity -- no node in the cluster satisfies. Those are the ones that
keep a pod Pending.

nodeSelector entries must all match; required nodeAffinity needs at least one term
to match, and within a term every expression must match. That structure is
evaluated faithfully, so the verdict per node is the real one.

Only labels are considered. Taints and tolerations, resource requests, topology
spread constraints and volume topology can equally block scheduling and are not
checked here.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, ns, err := g.build()
			if err != nil {
				return err
			}
			return g.emit(nodecheck.New(src, ns).Pod(cmd.Context(), args[0]))
		},
	}
}
