// Command kubectl-vmsnap resolves KubeVirt / OpenShift Virtualization snapshot
// and disk reference chains down to storage-array objects.
package main

import "github.com/mjovanovic0/kubectl-vmsnap/cmd"

func main() {
	cmd.Execute()
}
