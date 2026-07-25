package main

import (
	"errors"
	goflag "flag"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	utilflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"open-cluster-management.io/cluster-proxy/pkg/controllers"
	"open-cluster-management.io/cluster-proxy/pkg/serviceproxy"
	"open-cluster-management.io/cluster-proxy/pkg/userserver"
	"open-cluster-management.io/cluster-proxy/pkg/version"
)

func main() {
	klog.InitFlags(nil)
	pflag.CommandLine.SetNormalizeFunc(utilflag.WordSepNormalizeFunc)
	pflag.CommandLine.AddGoFlagSet(goflag.CommandLine)

	logs.InitLogs()
	os.Exit(runMain(execute, os.Stderr, logs.FlushLogs))
}

func runMain(executeCommand func() error, stderr io.Writer, flushLogs func()) int {
	defer flushLogs()

	if err := executeCommand(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	return 0
}

func execute() error {
	return newClusterProxyCommand().ExecuteContext(signals.SetupSignalHandler())
}

func newClusterProxyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster-proxy",
		Short: "cluster-proxy",
		// runMain prints returned errors; keep Cobra from printing them again.
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.Help(); err != nil {
				return err
			}
			// The help text above already covers usage.
			cmd.SilenceUsage = true
			return errors.New("a subcommand is required")
		},
	}

	if v := version.Get().String(); len(v) == 0 {
		cmd.Version = "<unknown>"
	} else {
		cmd.Version = v
	}

	cmd.AddCommand(userserver.NewUserServerCommand())
	cmd.AddCommand(serviceproxy.NewServiceProxyCommand())
	cmd.AddCommand(controllers.NewControllersCommand())

	return cmd
}
