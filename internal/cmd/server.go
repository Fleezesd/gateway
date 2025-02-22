// Copyright Envoy Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package cmd

import (
	"context"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"
	"github.com/envoyproxy/gateway/internal/admin"
	"github.com/envoyproxy/gateway/internal/envoygateway/config"
	"github.com/envoyproxy/gateway/internal/envoygateway/config/loader"
	extensionregistry "github.com/envoyproxy/gateway/internal/extension/registry"
	"github.com/envoyproxy/gateway/internal/extension/types"
	gatewayapirunner "github.com/envoyproxy/gateway/internal/gatewayapi/runner"
	ratelimitrunner "github.com/envoyproxy/gateway/internal/globalratelimit/runner"
	infrarunner "github.com/envoyproxy/gateway/internal/infrastructure/runner"
	"github.com/envoyproxy/gateway/internal/logging"
	"github.com/envoyproxy/gateway/internal/message"
	"github.com/envoyproxy/gateway/internal/metrics"
	providerrunner "github.com/envoyproxy/gateway/internal/provider/runner"
	xdsserverrunner "github.com/envoyproxy/gateway/internal/xds/server/runner"
	xdstranslatorrunner "github.com/envoyproxy/gateway/internal/xds/translator/runner"
)

type Runner interface {
	Start(context.Context) error
	Name() string
}

// cfgPath is the path to the EnvoyGateway configuration file.
var cfgPath string

// getServerCommand returns the server cobra command to be executed.
func getServerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "server",
		Aliases: []string{"serve"},
		Short:   "Serve Envoy Gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			return server()
		},
	}
	cmd.PersistentFlags().StringVarP(&cfgPath, "config-path", "c", "",
		"The path to the configuration file.")

	return cmd
}

// server serves Envoy Gateway.
func server() error {
	cfg, err := getConfig()
	if err != nil {
		return err
	}

	ctx := ctrl.SetupSignalHandler()
	hook := func(c context.Context, cfg *config.Server) error {
		cfg.Logger.Info("Setup runners")
		if err := setupRunners(c, cfg); err != nil {
			cfg.Logger.Error(err, "failed to setup runners")
			return err
		}
		return nil
	}
	l := loader.New(cfgPath, cfg, hook)
	if err := l.Start(ctx); err != nil {
		return err
	}

	// Init eg admin servers.
	if err := admin.Init(cfg); err != nil {
		return err
	}
	// Init eg metrics servers.
	if err := metrics.Init(cfg); err != nil {
		return err
	}

	// Wait exit signal
	<-ctx.Done()

	cfg.Logger.Info("shutting down")

	return nil
}

// getConfig gets the Server configuration
func getConfig() (*config.Server, error) {
	return getConfigByPath(cfgPath)
}

// make `cfgPath` an argument to test it without polluting the global var
func getConfigByPath(cfgPath string) (*config.Server, error) {
	// Initialize with default config parameters.
	cfg, err := config.New()
	if err != nil {
		return nil, err
	}

	logger := cfg.Logger

	// Read the config file.
	if cfgPath == "" {
		// Use default config parameters
		logger.Info("No config file provided, using default parameters")
	} else {
		// Load the config file.
		eg, err := config.Decode(cfgPath)
		if err != nil {
			logger.Error(err, "failed to decode config file", "name", cfgPath)
			return nil, err
		}
		// Set defaults for unset fields
		eg.SetEnvoyGatewayDefaults()
		cfg.EnvoyGateway = eg
		// update cfg logger
		eg.Logging.SetEnvoyGatewayLoggingDefaults()
		cfg.Logger = logging.NewLogger(eg.Logging)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// setupRunners starts all the runners required for the Envoy Gateway to
// fulfill its tasks.
func setupRunners(ctx context.Context, cfg *config.Server) (err error) {
	channels := struct {
		pResources *message.ProviderResources
		xdsIR      *message.XdsIR
		infraIR    *message.InfraIR
		xds        *message.Xds
	}{
		pResources: new(message.ProviderResources),
		xdsIR:      new(message.XdsIR),
		infraIR:    new(message.InfraIR),
		xds:        new(message.Xds),
	}

	// The Elected channel is used to block the tasks that are waiting for the leader to be elected.
	// It will be closed once the leader is elected in the controller manager.
	cfg.Elected = make(chan struct{})

	// Setup the Extension Manager
	var extMgr types.Manager
	if cfg.EnvoyGateway.Provider.Type == egv1a1.ProviderTypeKubernetes {
		if extMgr, err = extensionregistry.NewManager(cfg); err != nil {
			return err
		}
	}

	runners := []struct {
		runner Runner
	}{
		{
			// Start the Provider Service
			// It fetches the resources from the configured provider type
			// and publishes it.
			// It also subscribes to status resources and once it receives
			// a status resource back, it writes it out.
			runner: providerrunner.New(&providerrunner.Config{
				Server:            *cfg,
				ProviderResources: channels.pResources,
			}),
		},
		{
			// Start the GatewayAPI Translator Runner
			// It subscribes to the provider resources, translates it to xDS IR
			// and infra IR resources and publishes them.
			runner: gatewayapirunner.New(&gatewayapirunner.Config{
				Server:            *cfg,
				ProviderResources: channels.pResources,
				XdsIR:             channels.xdsIR,
				InfraIR:           channels.infraIR,
				ExtensionManager:  extMgr,
			}),
		},
		{
			// Start the Xds Translator Service
			// It subscribes to the xdsIR, translates it into xds Resources and publishes it.
			// It also computes the EnvoyPatchPolicy statuses and publishes it.
			runner: xdstranslatorrunner.New(&xdstranslatorrunner.Config{
				Server:            *cfg,
				XdsIR:             channels.xdsIR,
				Xds:               channels.xds,
				ExtensionManager:  extMgr,
				ProviderResources: channels.pResources,
			}),
		},
		{
			// Start the Infra Manager Runner
			// It subscribes to the infraIR, translates it into Envoy Proxy infrastructure
			// resources such as K8s deployment and services.
			runner: infrarunner.New(&infrarunner.Config{
				Server:  *cfg,
				InfraIR: channels.infraIR,
			}),
		},
		{
			// Start the xDS Server
			// It subscribes to the xds Resources and configures the remote Envoy Proxy
			// via the xDS Protocol.
			runner: xdsserverrunner.New(&xdsserverrunner.Config{
				Server: *cfg,
				Xds:    channels.xds,
			}),
		},
	}

	// Start all runners
	for _, r := range runners {
		if err = startRunner(ctx, cfg, r.runner); err != nil {
			return err
		}
	}
	// Start the global rateLimit if it has been enabled through the config
	if cfg.EnvoyGateway.RateLimit != nil {
		// Start the Global RateLimit xDS Server
		// It subscribes to the xds Resources and translates it to Envoy Ratelimit configuration.
		rateLimitRunner := ratelimitrunner.New(&ratelimitrunner.Config{
			Server: *cfg,
			XdsIR:  channels.xdsIR,
		})
		if err = startRunner(ctx, cfg, rateLimitRunner); err != nil {
			return err
		}
	}

	// Wait until done
	<-ctx.Done()

	// Close messages
	closeChannels := []interface{ Close() }{
		channels.pResources,
		channels.xdsIR,
		channels.infraIR,
		channels.xds,
	}
	for _, ch := range closeChannels {
		ch.Close()
	}

	cfg.Logger.Info("runners are shutting down")

	if extMgr != nil {
		// Close connections to extension services
		if mgr, ok := extMgr.(*extensionregistry.Manager); ok {
			mgr.CleanupHookConns()
		}
	}

	return nil
}

func startRunner(ctx context.Context, cfg *config.Server, runner Runner) error {
	cfg.Logger.Info("Starting runner", "name", runner.Name())
	if err := runner.Start(ctx); err != nil {
		cfg.Logger.Error(err, "Failed to start runner", "name", runner.Name())
		return err
	}
	return nil
}
