package provisioner

import (
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	addonclient "open-cluster-management.io/api/client/addon/clientset/versioned"
)

func NewManagedKubeconfigProvisionerCommand() *cobra.Command {
	options := Options{}

	cmd := &cobra.Command{
		Use:   "managed-kubeconfig-provisioner",
		Short: "Provision a minimal managed-cluster kubeconfig for hosted mode",
		RunE: func(cmd *cobra.Command, args []string) error {
			options.Complete()
			if err := options.Validate(); err != nil {
				return err
			}

			config, err := rest.InClusterConfig()
			if err != nil {
				return err
			}
			hostingClient, err := kubernetes.NewForConfig(rest.AddUserAgent(config, "cluster-proxy-managed-kubeconfig-provisioner"))
			if err != nil {
				return err
			}

			provisioner := NewProvisioner(options, hostingClient)
			if options.HubKubeconfig != "" {
				hubConfig, err := clientcmd.BuildConfigFromFlags("", options.HubKubeconfig)
				if err != nil {
					return err
				}
				hubAddonClient, err := addonclient.NewForConfig(rest.AddUserAgent(hubConfig, "cluster-proxy-managed-kubeconfig-provisioner"))
				if err != nil {
					return err
				}
				provisioner.WithAddonClient(hubAddonClient)
			}
			return provisioner.Run(cmd.Context())
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&options.ClusterName, "cluster-name", options.ClusterName, "The managed cluster name")
	flags.StringVar(&options.SourceNamespace, "source-namespace", options.SourceNamespace, "The namespace of the external managed kubeconfig secret. Defaults to --cluster-name")
	flags.StringVar(&options.SourceName, "source-name", DefaultSourceSecretName, "The external managed kubeconfig secret name")
	flags.StringVar(&options.TargetNamespace, "target-namespace", options.TargetNamespace, "The namespace for the generated managed kubeconfig secret. Defaults to POD_NAMESPACE")
	flags.StringVar(&options.TargetName, "target-name", DefaultTargetSecretName, "The generated managed kubeconfig secret name")
	flags.StringVar(&options.ManagedServiceAccountNamespace, "managed-service-account-namespace", options.ManagedServiceAccountNamespace, "The namespace of the managed cluster service account. Defaults to --target-namespace")
	flags.StringVar(&options.ManagedServiceAccountName, "managed-service-account-name", DefaultManagedServiceAccountName, "The managed cluster service account name")
	flags.DurationVar(&options.TokenExpiration, "token-expiration", DefaultTokenExpiration, "Requested TokenRequest expiration")
	flags.DurationVar(&options.RefreshBefore, "refresh-before", DefaultRefreshBefore, "Refresh the managed kubeconfig this long before token expiration")
	flags.DurationVar(&options.SyncInterval, "sync-interval", DefaultSyncInterval, "Interval between managed kubeconfig syncs")
	flags.StringVar(&options.HubKubeconfig, "hub-kubeconfig", options.HubKubeconfig, "The kubeconfig file for connecting to the hub cluster")
	flags.StringVar(&options.AddonName, "addon-name", DefaultAddonName, "The ManagedClusterAddOn name to patch with managed kubeconfig readiness")
	flags.StringVar(&options.AddonNamespace, "addon-namespace", options.AddonNamespace, "The ManagedClusterAddOn namespace to patch. Defaults to --cluster-name")
	flags.StringVar(&options.HealthProbeBindAddress, "health-probe-bind-address", DefaultHealthProbeBindAddress, "The address the health probe and metrics endpoint binds to")
	flags.BoolVar(&options.Cleanup, "cleanup", false, "Delete the generated managed kubeconfig secret and exit")

	return cmd
}
