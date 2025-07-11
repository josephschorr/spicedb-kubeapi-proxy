package proxy

import (
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/grpcutil"
	"github.com/authzed/spicedb/pkg/cmd/datastore"
	"github.com/authzed/spicedb/pkg/cmd/server"
	"github.com/authzed/spicedb/pkg/cmd/util"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"k8s.io/apiserver/pkg/endpoints/request"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/clientcmd"
	logsv1 "k8s.io/component-base/logs/api/v1"
)

func TestKubeConfig(t *testing.T) {
	defer require.NoError(t, logsv1.ResetForTest(utilfeature.DefaultFeatureGate))

	opts := optionsForTesting(t)
	opts.SpiceDBOptions.SpiceDBEndpoint = EmbeddedSpiceDBEndpoint
	require.Empty(t, opts.Validate())

	c, err := opts.Complete(context.Background())
	require.NoError(t, err)
	require.NotNil(t, c)

	require.NoError(t, logsv1.ResetForTest(utilfeature.DefaultFeatureGate))
	opts = optionsForTesting(t)
	opts.BackendKubeconfigPath = uuid.NewString()

	c, err = opts.Complete(context.Background())
	require.ErrorContains(t, err, "couldn't load kubeconfig")
	require.ErrorContains(t, err, opts.BackendKubeconfigPath)
	require.Nil(t, c, "expected nil config on error")
}

func TestInClusterConfig(t *testing.T) {
	defer require.NoError(t, logsv1.ResetForTest(utilfeature.DefaultFeatureGate))

	opts := optionsForTesting(t)
	opts.SpiceDBOptions.SpiceDBEndpoint = EmbeddedSpiceDBEndpoint
	opts.BackendKubeconfigPath = ""
	opts.UseInClusterConfig = true
	require.Empty(t, opts.Validate())

	c, err := opts.Complete(context.Background())
	require.NoError(t, err)
	require.NotNil(t, c)
	require.NotNil(t, opts.RestConfigFunc, "missing kube client REST config")

	_, _, err = opts.RestConfigFunc()
	require.ErrorContains(t, err, "unable to load in-cluster configuration")
}

func TestEmbeddedSpiceDB(t *testing.T) {
	opts := optionsForTesting(t)
	opts.SpiceDBOptions.SpiceDBEndpoint = EmbeddedSpiceDBEndpoint
	require.Empty(t, opts.Validate())

	c, err := opts.Complete(context.Background())
	require.NoError(t, err)
	require.NotNil(t, c)

	require.NotNil(t, opts.SpiceDBOptions.EmbeddedSpiceDB)
	require.NotNil(t, opts.PermissionsClient)
	require.NotNil(t, opts.WatchClient)
}

func TestRemoteSpiceDB(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, addr := newTCPSpiceDB(t, ctx)
	go func() {
		if err := srv.Run(ctx); err != nil {
			require.NoError(t, err)
		}
	}()

	opts := optionsForTesting(t)
	opts.SpiceDBOptions.SpiceDBEndpoint = addr
	opts.SpiceDBOptions.Insecure = true
	opts.SpiceDBOptions.SecureSpiceDBTokensBySpace = "foobar"
	require.Empty(t, opts.Validate())

	c, err := opts.Complete(context.Background())
	require.NoError(t, err)
	require.NotNil(t, c)

	require.Nil(t, opts.SpiceDBOptions.EmbeddedSpiceDB)
	require.NotNil(t, opts.PermissionsClient)
	require.NotNil(t, opts.WatchClient)

	_, err = opts.PermissionsClient.CheckPermission(ctx, &v1.CheckPermissionRequest{})
	grpcutil.RequireStatus(t, codes.InvalidArgument, err)
}

func TestRemoteSpiceDBCerts(t *testing.T) {
	opts := optionsForTesting(t)
	opts.SpiceDBOptions.SpiceDBEndpoint = "localhost"
	opts.SpiceDBOptions.SecureSpiceDBTokensBySpace = "foobar"
	opts.SpiceDBOptions.SpicedbCAPath = "test"
	require.Empty(t, opts.Validate())

	_, err := opts.Complete(context.Background())
	require.ErrorContains(t, err, "unable to load custom certificates")
}

func TestRuleConfig(t *testing.T) {
	opts := optionsForTesting(t)
	opts.SpiceDBOptions.SpiceDBEndpoint = EmbeddedSpiceDBEndpoint
	require.Empty(t, opts.Validate())

	c, err := opts.Complete(context.Background())
	require.NoError(t, err)
	require.NotNil(t, c)

	rules := opts.Matcher.Match(&request.RequestInfo{
		APIGroup:   "authzed.com",
		APIVersion: "v1alpha1",
		Resource:   "spicedbclusters",
		Verb:       "list",
	})
	require.Len(t, rules, 1)
	require.Len(t, rules[0].PreFilter, 1)
	require.Len(t, rules[0].Checks, 0)
	require.Nil(t, rules[0].Update)

	require.NoError(t, logsv1.ResetForTest(utilfeature.DefaultFeatureGate))
	errConfigBytes := []byte(`
apiVersion: authzed.com/v1alpha1
kind: ProxyRule
lock: Pessimistic 
match:
- apiVersion: authzed.com/v1alpha1
  resource: spicedbclusters
  verbs: ["list"]
prefilter:
- fromObjectIDNameExpr: "{{invalid bloblang syntax}}"
  lookupMatchingResources:
    tpl: "org:$#audit-cluster@user:{{request.user}}"
`)
	errConfigFile := path.Join(t.TempDir(), "rulesbad.yaml")
	require.NoError(t, os.WriteFile(errConfigFile, errConfigBytes, 0o600))
	opts = optionsForTesting(t)
	opts.SpiceDBOptions.SpiceDBEndpoint = EmbeddedSpiceDBEndpoint
	opts.RuleConfigFile = errConfigFile
	require.Empty(t, opts.Validate())

	_, err = opts.Complete(context.Background())
	require.ErrorContains(t, err, "expected")
}

func optionsForTesting(t *testing.T) *Options {
	t.Helper()

	require.NoError(t, logsv1.ResetForTest(utilfeature.DefaultFeatureGate))
	opts := NewOptions()
	opts.SecureServing.BindPort = getFreePort(t, "127.0.0.1")
	opts.SecureServing.BindAddress = net.ParseIP("127.0.0.1")
	opts.BackendKubeconfigPath = kubeConfigForTest(t)
	opts.RuleConfigFile = ruleConfigForTest(t)
	require.Empty(t, opts.Validate())
	return opts
}

func getFreePort(t *testing.T, listenAddr string) int {
	t.Helper()

	dummyListener, err := net.Listen("tcp", net.JoinHostPort(listenAddr, "0"))
	require.NoError(t, err)

	defer require.NoError(t, dummyListener.Close())
	port := dummyListener.Addr().(*net.TCPAddr).Port
	return port
}

func newTCPSpiceDB(t *testing.T, ctx context.Context) (server.RunnableServer, string) {
	t.Helper()

	ds, err := datastore.NewDatastore(ctx,
		datastore.DefaultDatastoreConfig().ToOption(),
		datastore.WithRequestHedgingEnabled(false),
	)
	require.NoError(t, err)

	port := getFreePort(t, "localhost")
	address := fmt.Sprintf("localhost:%d", port)

	configOpts := []server.ConfigOption{
		server.WithGRPCServer(util.GRPCServerConfig{
			Network: "tcp",
			Address: address,
			Enabled: true,
		}),
		server.WithPresharedSecureKey("foobar"),
		server.WithHTTPGateway(util.HTTPServerConfig{HTTPEnabled: false}),
		server.WithMetricsAPI(util.HTTPServerConfig{HTTPEnabled: false}),
		// disable caching since it's all in memory
		server.WithDispatchCacheConfig(server.CacheConfig{Enabled: false, Metrics: false}),
		server.WithNamespaceCacheConfig(server.CacheConfig{Enabled: false, Metrics: false}),
		server.WithClusterDispatchCacheConfig(server.CacheConfig{Enabled: false, Metrics: false}),
		server.WithDatastore(ds),
	}

	srv, err := server.NewConfigWithOptionsAndDefaults(configOpts...).Complete(ctx)
	require.NoError(t, err)

	return srv, address
}

func kubeConfigForTest(t *testing.T) string {
	t.Helper()

	c, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	require.NoError(t, err)
	f, err := os.CreateTemp("", "spicedb-kubeapi-proxy")
	require.NoError(t, err)

	err = clientcmd.WriteToFile(*c, f.Name())
	require.NoError(t, err)

	return f.Name()
}

func ruleConfigForTest(t *testing.T) string {
	t.Helper()

	configBytes := []byte(`
apiVersion: authzed.com/v1alpha1
kind: ProxyRule
lock: Pessimistic 
match:
- apiVersion: authzed.com/v1alpha1
  resource: spicedbclusters 
  verbs: ["list"]
prefilter:
- fromObjectIDNameExpr: "{{request.name}}"
  lookupMatchingResources:
    tpl: "org:$#audit-cluster@user:{{request.user}}"
`)
	configFile := path.Join(t.TempDir(), "rules.yaml")
	require.NoError(t, os.WriteFile(configFile, configBytes, 0o600))
	return configFile
}
