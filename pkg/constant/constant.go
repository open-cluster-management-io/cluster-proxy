package constant

const (
	AgentInstallNamespace = "open-cluster-management-agent-addon"

	ServiceProxyPort = 7443

	ServiceRelayPort = 7444

	ServerCertSecretName = "cluster-proxy-service-proxy-server-cert"

	ServiceProxyName = "cluster-proxy-service-proxy"

	ServiceRelayName = "cluster-proxy-service-relay"

	AddonName = "cluster-proxy"

	// UserServerSecretName is the fixed secret name for user server certificates.
	// This is used both by controller-generated certificates and external certificate generators
	// to ensure consistency.
	UserServerSecretName = "cluster-proxy-user-serving-cert"

	// UserServerServiceName is the fixed service name for user server.
	UserServerServiceName = "cluster-proxy-addon-user"
)
