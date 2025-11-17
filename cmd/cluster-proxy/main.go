package main

import (
	goflag "flag"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	utilflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"

	"open-cluster-management.io/cluster-proxy/pkg/controllers"
	"open-cluster-management.io/cluster-proxy/pkg/serviceproxy"
	"open-cluster-management.io/cluster-proxy/pkg/userserver"
	"open-cluster-management.io/cluster-proxy/pkg/version"
)

func main() {
	rand.Seed(time.Now().UTC().UnixNano())

	klog.InitFlags(nil)
	pflag.CommandLine.SetNormalizeFunc(utilflag.WordSepNormalizeFunc)
	pflag.CommandLine.AddGoFlagSet(goflag.CommandLine)

	logs.InitLogs()
	defer logs.FlushLogs()

	command := newClusterProxyCommand()
	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func newClusterProxyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster-proxy",
		Short: "cluster-proxy",
		Run: func(cmd *cobra.Command, args []string) {
			if err := cmd.Help(); err != nil {
				klog.Errorf("cmd help err: %v", err)
			}
			os.Exit(1)
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
