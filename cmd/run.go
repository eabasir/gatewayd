package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	gerr "github.com/gatewayd-io/gatewayd/errors"
	"github.com/gatewayd-io/gatewayd/logging"
	"github.com/gatewayd-io/gatewayd/network"
	"github.com/gatewayd-io/gatewayd/plugin"
	"github.com/gatewayd-io/gatewayd/pool"
	goplugin "github.com/hashicorp/go-plugin"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/panjf2000/gnet/v2"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	DefaultTCPKeepAlive = 3 * time.Second
)

var (
	globalConfigFile string
	pluginConfigFile string
)

var (
	hooksConfig    = plugin.NewHookConfig()
	DefaultLogger  = logging.NewLogger(logging.LoggerConfig{Level: zerolog.DebugLevel})
	pluginRegistry = plugin.NewRegistry(hooksConfig)
)

// runCmd represents the run command.
var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run a gatewayd instance",
	Run: func(cmd *cobra.Command, args []string) {
		// The plugins are loaded and hooks registered
		// before the configuration is loaded.
		hooksConfig.Logger = DefaultLogger

		// Load the plugin configuration file
		if f, err := cmd.Flags().GetString("plugin-config"); err == nil {
			if err := pluginConfig.Load(file.Provider(f), yaml.Parser()); err != nil {
				DefaultLogger.Fatal().Err(err).Msg("Failed to load plugin configuration")
				os.Exit(gerr.FailedToLoadPluginConfig)
			}
		}

		// Load plugins and register their hooks
		pluginRegistry.LoadPlugins(pluginConfig)

		if f, err := cmd.Flags().GetString("config"); err == nil {
			if err := globalConfig.Load(file.Provider(f), yaml.Parser()); err != nil {
				DefaultLogger.Fatal().Err(err).Msg("Failed to load configuration")
				os.Exit(gerr.FailedToLoadGlobalConfig)
			}
		}

		// Get hooks signature verification policy
		hooksConfig.Verification = verificationPolicy()

		// The config will be passed to the hooks, and in turn to the plugins that
		// register to this hook.
		currentGlobalConfig, err := structpb.NewStruct(globalConfig.All())
		if err != nil {
			DefaultLogger.Error().Err(err).Msg("Failed to convert configuration to structpb")
		} else {
			updatedGlobalConfig, _ := hooksConfig.Run(
				context.Background(),
				currentGlobalConfig,
				plugin.OnConfigLoaded,
				hooksConfig.Verification)

			if updatedGlobalConfig != nil && plugin.Verify(updatedGlobalConfig, currentGlobalConfig) {
				// Merge the config with the one loaded from the file (in memory).
				// The changes won't be persisted to disk.
				if err := globalConfig.Load(
					confmap.Provider(updatedGlobalConfig.AsMap(), "."), nil); err != nil {
					DefaultLogger.Fatal().Err(err).Msg("Failed to merge configuration")
				}
			}
		}

		// Create a new logger from the config
		loggerCfg := loggerConfig()
		logger := logging.NewLogger(loggerCfg)
		// TODO: Use https://github.com/dcarbone/zadapters to adapt hclog to zerolog
		// This is a notification hook, so we don't care about the result.
		data, err := structpb.NewStruct(map[string]interface{}{
			"timeFormat": loggerCfg.TimeFormat,
			"level":      loggerCfg.Level,
			"output":     loggerCfg.Output,
			"noColor":    loggerCfg.NoColor,
		})
		if err != nil {
			logger.Error().Err(err).Msg("Failed to convert logger config to structpb")
		} else {
			// TODO: Use a context with a timeout
			_, err := hooksConfig.Run(
				context.Background(), data, plugin.OnNewLogger, hooksConfig.Verification)
			if err != nil {
				logger.Error().Err(err).Msg("Failed to run OnNewLogger hooks")
			}
		}

		// Create and initialize a pool of connections
		pool := pool.NewPool()
		poolSize, clientConfig := poolConfig()

		// Add clients to the pool
		for i := 0; i < poolSize; i++ {
			client := network.NewClient(
				clientConfig.Network,
				clientConfig.Address,
				clientConfig.ReceiveBufferSize,
				logger,
			)

			if client != nil {
				clientCfg, err := structpb.NewStruct(map[string]interface{}{
					"id":                client.ID,
					"network":           clientConfig.Network,
					"address":           clientConfig.Address,
					"receiveBufferSize": clientConfig.ReceiveBufferSize,
				})
				if err != nil {
					logger.Error().Err(err).Msg("Failed to convert client config to structpb")
				} else {
					_, err := hooksConfig.Run(
						context.Background(),
						clientCfg,
						plugin.OnNewClient,
						hooksConfig.Verification)
					if err != nil {
						logger.Error().Err(err).Msg("Failed to run OnNewClient hooks")
					}
				}

				pool.Put(client.ID, client)
			}
		}

		// Verify that the pool is properly populated
		logger.Info().Msgf("There are %d clients in the pool", pool.Size())
		if pool.Size() != poolSize {
			logger.Error().Msg(
				"The pool size is incorrect, either because " +
					"the clients cannot connect due to no network connectivity " +
					"or the server is not running. exiting...")
			os.Exit(1)
		}

		poolCfg, err := structpb.NewStruct(map[string]interface{}{
			"size": poolSize,
		})
		if err != nil {
			logger.Error().Err(err).Msg("Failed to convert pool config to structpb")
		} else {
			_, err := hooksConfig.Run(
				context.Background(), poolCfg, plugin.OnNewPool, hooksConfig.Verification)
			if err != nil {
				logger.Error().Err(err).Msg("Failed to run OnNewPool hooks")
			}
		}

		// Create a prefork proxy with the pool of clients
		elastic, reuseElasticClients, elasticClientConfig := proxyConfig()
		proxy := network.NewProxy(
			pool, hooksConfig, elastic, reuseElasticClients, elasticClientConfig, logger)

		proxyCfg, err := structpb.NewStruct(map[string]interface{}{
			"elastic":             elastic,
			"reuseElasticClients": reuseElasticClients,
			"clientConfig":        elasticClientConfig,
		})
		if err != nil {
			logger.Error().Err(err).Msg("Failed to convert proxy config to structpb")
		} else {
			_, err := hooksConfig.Run(
				context.Background(), proxyCfg, plugin.OnNewProxy, hooksConfig.Verification)
			if err != nil {
				logger.Error().Err(err).Msg("Failed to run OnNewProxy hooks")
			}
		}

		// Create a server
		serverConfig := serverConfig()
		server := network.NewServer(
			serverConfig.Network,
			serverConfig.Address,
			serverConfig.SoftLimit,
			serverConfig.HardLimit,
			serverConfig.TickInterval,
			[]gnet.Option{
				// Scheduling options
				gnet.WithMulticore(serverConfig.MultiCore),
				gnet.WithLockOSThread(serverConfig.LockOSThread),
				// NumEventLoop overrides Multicore option.
				// gnet.WithNumEventLoop(1),

				// Can be used to send keepalive messages to the client.
				gnet.WithTicker(serverConfig.EnableTicker),

				// Internal event-loop load balancing options
				gnet.WithLoadBalancing(serverConfig.LoadBalancer),

				// Logger options
				// TODO: This is a temporary solution and will be replaced.
				// gnet.WithLogger(logrus.New()),
				// gnet.WithLogPath("./gnet.log"),
				// gnet.WithLogLevel(zapcore.DebugLevel),

				// Buffer options
				gnet.WithReadBufferCap(serverConfig.ReadBufferCap),
				gnet.WithWriteBufferCap(serverConfig.WriteBufferCap),
				gnet.WithSocketRecvBuffer(serverConfig.SocketRecvBuffer),
				gnet.WithSocketSendBuffer(serverConfig.SocketSendBuffer),

				// TCP options
				gnet.WithReuseAddr(serverConfig.ReuseAddress),
				gnet.WithReusePort(serverConfig.ReusePort),
				gnet.WithTCPKeepAlive(serverConfig.TCPKeepAlive),
				gnet.WithTCPNoDelay(serverConfig.TCPNoDelay),
			},
			proxy,
			logger,
			hooksConfig,
		)

		serverCfg, err := structpb.NewStruct(map[string]interface{}{
			"network":          serverConfig.Network,
			"address":          serverConfig.Address,
			"softLimit":        serverConfig.SoftLimit,
			"hardLimit":        serverConfig.HardLimit,
			"tickInterval":     serverConfig.TickInterval,
			"multiCore":        serverConfig.MultiCore,
			"lockOSThread":     serverConfig.LockOSThread,
			"enableTicker":     serverConfig.EnableTicker,
			"loadBalancer":     serverConfig.LoadBalancer,
			"readBufferCap":    serverConfig.ReadBufferCap,
			"writeBufferCap":   serverConfig.WriteBufferCap,
			"socketRecvBuffer": serverConfig.SocketRecvBuffer,
			"socketSendBuffer": serverConfig.SocketSendBuffer,
			"reuseAddress":     serverConfig.ReuseAddress,
			"reusePort":        serverConfig.ReusePort,
			"tcpKeepAlive":     serverConfig.TCPKeepAlive,
			"tcpNoDelay":       serverConfig.TCPNoDelay,
		})
		if err != nil {
			logger.Error().Err(err).Msg("Failed to convert server config to structpb")
		} else {
			_, err := hooksConfig.Run(
				context.Background(), serverCfg, plugin.OnNewServer, hooksConfig.Verification)
			if err != nil {
				logger.Error().Err(err).Msg("Failed to run OnNewServer hooks")
			}
		}
		// Shutdown the server gracefully
		var signals []os.Signal
		signals = append(signals,
			os.Interrupt,
			os.Kill,
			syscall.SIGTERM,
			syscall.SIGABRT,
			syscall.SIGQUIT,
			syscall.SIGHUP,
			syscall.SIGINT,
		)
		signalsCh := make(chan os.Signal, 1)
		signal.Notify(signalsCh, signals...)
		go func(hooksConfig *plugin.HookConfig) {
			for sig := range signalsCh {
				for _, s := range signals {
					if sig != s {
						// Notify the hooks that the server is shutting down
						signalCfg, err := structpb.NewStruct(map[string]interface{}{"signal": sig})
						if err != nil {
							logger.Error().Err(err).Msg(
								"Failed to convert signal config to structpb")
						} else {
							_, err := hooksConfig.Run(
								context.Background(),
								signalCfg,
								plugin.OnSignal,
								hooksConfig.Verification,
							)
							if err != nil {
								logger.Error().Err(err).Msg("Failed to run OnSignal hooks")
							}
						}

						server.Shutdown()
						pluginRegistry.Shutdown()
						goplugin.CleanupClients()
						os.Exit(0)
					}
				}
			}
		}(hooksConfig)

		// Run the server
		if err := server.Run(); err != nil {
			logger.Error().Err(err).Msg("Failed to start server")
		}
	},
}

func init() {
	rootCmd.AddCommand(runCmd)

	runCmd.PersistentFlags().StringVarP(
		&globalConfigFile,
		"config", "c", "./gatewayd.yaml",
		"config file (default is ./gatewayd.yaml)")
	runCmd.PersistentFlags().StringVarP(
		&pluginConfigFile,
		"plugin-config", "p", "./gatewayd_plugins.yaml",
		"plugin config file (default is ./gatewayd_plugins.yaml)")
}