package spicedb

import (
	"context"
	_ "embed"
	"time"

	"github.com/authzed/spicedb/pkg/cmd/datastore"
	"github.com/authzed/spicedb/pkg/cmd/server"
	"github.com/authzed/spicedb/pkg/cmd/util"
	"github.com/dustin/go-humanize"
)

//go:embed bootstrap.yaml
var bootstrap []byte

func NewServer(ctx context.Context, bootstrapFilePath string) (server.RunnableServer, error) {
	bootstrapOption := datastore.SetBootstrapFileContents(map[string][]byte{"schema": bootstrap})
	if bootstrapFilePath != "" {
		bootstrapOption = datastore.SetBootstrapFiles([]string{bootstrapFilePath})
	}
	return server.NewConfigWithOptionsAndDefaults(server.WithGRPCServer(util.GRPCServerConfig{
		Network:    util.BufferedNetwork,
		Enabled:    true,
		BufferSize: 10 * humanize.MiByte,
	}),
		server.WithDispatchServer(util.GRPCServerConfig{Enabled: false}),
		server.WithDispatchUpstreamAddr(""),
		server.WithHTTPGatewayUpstreamAddr(""),
		server.WithDispatchMaxDepth(50),
		server.WithMaximumUpdatesPerWrite(1000),
		server.WithMaximumPreconditionCount(1000),
		server.WithMaxCaveatContextSize(1000000),
		server.WithMaxRelationshipContextSize(1000000),
		server.WithSchemaPrefixesRequired(false),
		server.WithHTTPGateway(util.HTTPServerConfig{HTTPEnabled: false}),
		server.WithMetricsAPI(util.HTTPServerConfig{HTTPEnabled: false}),
		server.WithSilentlyDisableTelemetry(true),
		server.WithDispatchClusterMetricsEnabled(false),
		server.WithDispatchClientMetricsEnabled(false),
		server.WithDispatchCacheConfig(server.CacheConfig{Enabled: false, Metrics: false}),
		server.WithNamespaceCacheConfig(server.CacheConfig{Enabled: false, Metrics: false}),
		server.WithClusterDispatchCacheConfig(server.CacheConfig{Enabled: false, Metrics: false}),
		server.WithDatastoreConfig(
			*datastore.NewConfigWithOptionsAndDefaults().WithOptions(
				datastore.WithEngine(datastore.MemoryEngine),
				bootstrapOption,
				datastore.WithRequestHedgingEnabled(false),
				datastore.WithGCWindow(24*time.Hour),
			)),
		server.WithGRPCAuthFunc(func(ctx context.Context) (context.Context, error) { return ctx, nil }),
	).Complete(ctx)
}
