package engine

import (
	"errors"
	"net"
	"net/http"

	"github.com/go-gost/core/logger"
	"github.com/go-gost/x/config"
	"github.com/go-gost/x/registry"
)

// run starts every service registered by loader.Load and (re)builds the API,
// metrics and profiling servers.
//
// It mirrors program.run in cmd/gost/program.go, with two differences: the
// previous API/metrics/profiling servers are closed before the new ones are
// created, and profiling/metrics are opt-in because an embedded library must
// not squat on ports the host did not ask for.
//
// Callers must hold e.mu.
func (e *Engine) run(cfg *config.Config) error {
	for _, svc := range registry.ServiceRegistry().GetAll() {
		go func() {
			if err := svc.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
				logger.Default().Debugf("service %s exited: %v", svc.Addr(), err)
			}
		}()
	}

	if err := e.runAPI(cfg); err != nil {
		return err
	}
	if err := e.runMetrics(cfg); err != nil {
		return err
	}
	return e.runProfiling(cfg)
}

func (e *Engine) runAPI(cfg *config.Config) error {
	if e.srvAPI != nil {
		e.srvAPI.Close()
		e.srvAPI = nil
	}
	if cfg.API == nil || cfg.API.Addr == "" {
		return nil
	}

	s, err := buildAPIService(cfg.API)
	if err != nil {
		return err
	}
	e.srvAPI = s

	go func() {
		log := logger.Default().WithFields(map[string]any{"kind": "service", "service": "@api"})
		log.Info("listening on ", s.Addr())
		if err := s.Serve(); !errors.Is(err, http.ErrServerClosed) {
			log.Error(err)
		}
	}()

	return nil
}

func (e *Engine) runMetrics(cfg *config.Config) error {
	if e.srvMetrics != nil {
		e.srvMetrics.Close()
		e.srvMetrics = nil
	}
	if cfg.Metrics == nil || cfg.Metrics.Addr == "" {
		return nil
	}

	s, err := buildMetricsService(cfg.Metrics)
	if err != nil {
		return err
	}
	e.srvMetrics = s

	go func() {
		log := logger.Default().WithFields(map[string]any{"kind": "service", "service": "@metrics"})
		log.Info("listening on ", s.Addr())
		if err := s.Serve(); !errors.Is(err, http.ErrServerClosed) {
			log.Error(err)
		}
	}()

	return nil
}

func (e *Engine) runProfiling(cfg *config.Config) error {
	if e.srvProfiling != nil {
		e.srvProfiling.Close()
		e.srvProfiling = nil
	}
	if cfg.Profiling == nil {
		return nil
	}

	addr := cfg.Profiling.Addr
	if addr == "" {
		addr = ":6060"
	}

	s := &http.Server{Addr: addr}
	e.srvProfiling = s

	go func() {
		log := logger.Default().WithFields(map[string]any{"kind": "service", "service": "@profiling"})
		log.Info("listening on ", addr)
		if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Error(err)
		}
	}()

	return nil
}

// closeServers shuts down the API, metrics and profiling servers. Callers must
// hold e.mu.
func (e *Engine) closeServers() {
	if e.srvAPI != nil {
		e.srvAPI.Close()
		e.srvAPI = nil
		logger.Default().Debug("service @api shutdown")
	}
	if e.srvMetrics != nil {
		e.srvMetrics.Close()
		e.srvMetrics = nil
		logger.Default().Debug("service @metrics shutdown")
	}
	if e.srvProfiling != nil {
		e.srvProfiling.Close()
		e.srvProfiling = nil
		logger.Default().Debug("service @profiling shutdown")
	}
}

// closeServices stops every service currently in the registry. Unregister
// closes the value first (x/registry/registry.go), which releases the bound
// port, so a later Start can rebind the same address.
func (e *Engine) closeServices() {
	for name, srv := range registry.ServiceRegistry().GetAll() {
		srv.Close()
		logger.Default().Debugf("service %s shutdown", name)
	}
}
