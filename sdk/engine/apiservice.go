package engine

import (
	"strings"

	"github.com/go-gost/core/auth"
	"github.com/go-gost/core/service"
	api_service "github.com/go-gost/x/api/service"
	xauth "github.com/go-gost/x/auth"
	"github.com/go-gost/x/config"
	auth_parser "github.com/go-gost/x/config/parsing/auth"
	metrics_service "github.com/go-gost/x/metrics/service"
	"github.com/go-gost/x/registry"
)

// buildAPIService builds the embedded Web API server. Ported from
// buildApiService in cmd/gost/program.go so the SDK behaves like the CLI,
// including unix:// socket support.
func buildAPIService(cfg *config.APIConfig) (service.Service, error) {
	var authers []auth.Authenticator
	if auther := auth_parser.ParseAutherFromAuth(cfg.Auth); auther != nil {
		authers = append(authers, auther)
	}
	if cfg.Auther != "" {
		authers = append(authers, registry.AutherRegistry().Get(cfg.Auther))
	}

	var auther auth.Authenticator
	if len(authers) > 0 {
		auther = xauth.AuthenticatorGroup(authers...)
	}

	network, addr := splitNetwork(cfg.Addr)
	return api_service.NewService(
		network, addr,
		api_service.PathPrefixOption(cfg.PathPrefix),
		api_service.AccessLogOption(cfg.AccessLog),
		api_service.AutherOption(auther),
	)
}

// buildMetricsService builds the Prometheus metrics server. Ported from
// buildMetricsService in cmd/gost/program.go.
func buildMetricsService(cfg *config.MetricsConfig) (service.Service, error) {
	auther := auth_parser.ParseAutherFromAuth(cfg.Auth)
	if cfg.Auther != "" {
		auther = registry.AutherRegistry().Get(cfg.Auther)
	}

	network, addr := splitNetwork(cfg.Addr)
	return metrics_service.NewService(
		network, addr,
		metrics_service.PathOption(cfg.Path),
		metrics_service.AutherOption(auther),
	)
}

// splitNetwork maps a "unix://path" address to the unix network, matching the
// behaviour of the CLI. Everything else stays on tcp and is returned verbatim.
func splitNetwork(addr string) (network, rest string) {
	network = "tcp"
	if strings.HasPrefix(addr, "unix://") {
		return "unix", strings.TrimPrefix(addr, "unix://")
	}
	return network, addr
}
