// Package engine drives an embedded GOST instance: it parses a configuration,
// loads it into GOST's global registries, runs the resulting services, and
// tears everything down again.
//
// The package is deliberately free of cgo so it can be unit tested; the
// c-shared entry points live in the module's main package.
//
// State is process-global — GOST's registries, the default logger and the
// metrics switch are all package level — so one process runs at most one
// instance. Engine is therefore normally a singleton.
package engine

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/go-gost/core/logger"
	"github.com/go-gost/core/service"
	"github.com/go-gost/x/config"
	"github.com/go-gost/x/config/loader"
	"github.com/go-gost/x/config/parsing/parser"
	xmetrics "github.com/go-gost/x/metrics"
	"github.com/go-gost/x/registry"
)

// version is stamped at build time via
// -ldflags "-X github.com/go-gost/sdk/engine.version=...".
var version = "3.3.0-20260907"

var (
	// ErrAlreadyRunning is returned by Start when an instance is already up.
	ErrAlreadyRunning = errors.New("gost: already running")
	// ErrNotRunning is returned by Reload when no instance is up.
	ErrNotRunning = errors.New("gost: not running")
)

// Version returns the stamped GOST version.
func Version() string { return version }

// Options configures a Start call.
type Options struct {
	// Config is the GOST configuration as JSON or YAML. When empty, GOST
	// falls back to the GOST_* environment variables and its default config
	// search paths.
	Config []byte
	// Debug enables debug-level logging.
	Debug bool
	// APIAddr optionally enables the embedded Web API on this address, e.g.
	// "127.0.0.1:18080". Empty means no API server, which is the safe default
	// for a mobile app: the API is unauthenticated unless the config supplies
	// credentials.
	APIAddr string
	// LogLevel is the minimum level forwarded to LogSink. The zero value is
	// LevelTrace, which forwards everything.
	LogLevel Level
	// LogSink receives every log entry, including lines that bypass the
	// structured logger and go straight to stderr. It is called from a
	// dedicated goroutine and may be nil, in which case entries go to the
	// process's original stderr.
	LogSink LogHandler
}

// Engine owns a running GOST instance.
type Engine struct {
	mu      sync.Mutex
	running bool
	config  []byte
	debug   bool
	apiAddr string

	// cfgPath is the staged copy of opts.Config. The parser needs a path so
	// that duration strings survive viper's decode hook, and POST
	// /config/reload re-reads the same file.
	cfgPath string

	emitter *emitter

	srvAPI       service.Service
	srvMetrics   service.Service
	srvProfiling *http.Server

	// defaultLogger writes to the log pipe. loader.Load replaces it on every
	// load; this is what the engine restores afterwards.
	defaultLogger logger.Logger
}

// New returns an Engine. It does not start anything.
func New() *Engine {
	return &Engine{emitter: newEmitter()}
}

// Start parses opts.Config, loads it and begins serving. Starting an already
// running engine returns ErrAlreadyRunning rather than implicitly restarting:
// the caller needs to know when the previous listeners were actually released.
func (e *Engine) Start(opts Options) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.running {
		return ErrAlreadyRunning
	}

	if !opts.LogLevel.Valid() {
		opts.LogLevel = LevelInfo
	}
	e.emitter.setLevel(opts.LogLevel)

	// The callback must be installed before anything can log.
	e.emitter.setHandler(opts.LogSink)
	if err := e.installLogging(); err != nil {
		e.emitter.reset()
		return fmt.Errorf("gost: install logging: %w", err)
	}

	cfg, err := e.parse(opts)
	if err != nil {
		e.rollback()
		return err
	}

	config.Set(cfg)

	// Metrics are forced off by parse, so make sure the process-wide switch
	// does not carry over from a previous run.
	xmetrics.Enable(false)

	// Seed the default logger before Load, which unconditionally overwrites it
	// with the logger built from the config.
	e.installLogger()

	if err := loader.Load(cfg); err != nil {
		e.rollback()
		return err
	}

	if err := e.run(cfg); err != nil {
		e.closeServers()
		e.closeServices()
		e.rollback()
		return err
	}

	e.config = opts.Config
	e.debug = opts.Debug
	e.apiAddr = opts.APIAddr
	e.running = true

	return nil
}

// parse turns the caller's configuration into a *config.Config.
//
// The configuration is staged to a temporary file and handed to
// x/config/parsing/parser as a config *path*. That indirection is what makes
// duration strings such as `"timeout": "10s"` work: the parser accepts an inline
// JSON string by calling encoding/json directly, which has no mapstructure
// decode hook and therefore rejects a string where a time.Duration is expected.
// Only the file path goes through config.ReadFile, which installs
// gostDecodeHook. Passing a path also keeps POST /config/reload working, since
// that handler re-runs parser.Parse() over the same arguments.
//
// Callers must hold e.mu.
func (e *Engine) parse(opts Options) (*config.Config, error) {
	path, err := e.stageConfig(opts.Config)
	if err != nil {
		return nil, err
	}

	args := parser.Args{Debug: opts.Debug, ApiAddr: opts.APIAddr}
	if path != "" {
		args.CfgFiles = []string{path}
	}
	parser.Init(args)

	cfg, err := parser.Parse()
	if err != nil {
		return nil, fmt.Errorf("gost: parse config: %w", err)
	}

	// Metrics and profiling stay off unless the host explicitly asked for
	// them: they are of little use in a mobile app and would hold ports.
	cfg.Metrics = nil
	cfg.Profiling = nil

	if opts.APIAddr != "" {
		if cfg.API == nil {
			cfg.API = &config.APIConfig{}
		}
		cfg.API.Addr = opts.APIAddr
	}

	return cfg, nil
}

// stageConfig writes the caller's configuration to a temporary file and returns
// its path. An empty config returns an empty path.
//
// Callers must hold e.mu.
func (e *Engine) stageConfig(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}

	if e.cfgPath == "" {
		f, err := os.CreateTemp("", "gost-*.json")
		if err != nil {
			return "", fmt.Errorf("gost: stage config: %w", err)
		}
		e.cfgPath = f.Name()
		f.Close()
	}

	if err := os.WriteFile(e.cfgPath, data, 0o600); err != nil {
		return "", fmt.Errorf("gost: stage config: %w", err)
	}

	return e.cfgPath, nil
}

// installLogger seeds the default logger before loader.Load runs. Load replaces
// it with the logger described by the config, which writes to whatever
// x/config/parsing/logger resolved — the log pipe, for the default
// configuration.
//
// Callers must hold e.mu.
func (e *Engine) installLogger() {
	if e.defaultLogger != nil {
		logger.SetDefault(e.defaultLogger)
	}
}

// rollback restores the state a failed Start or a Stop must leave behind: no
// host callback, no subscription to the log pipe, and a logger that does not
// write into it.
//
// Callers must hold e.mu.
func (e *Engine) rollback() {
	e.unsubscribe()
	e.emitter.reset()
	e.installLogger()
}

// Stop shuts the instance down. It is idempotent and never blocks on the log
// sink, so a host callback that calls Stop is safe.
func (e *Engine) Stop() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.running {
		return nil
	}
	e.running = false

	e.closeServers()
	e.closeServices()
	e.rollback()

	return nil
}

// Reload re-parses the configuration handed to the last Start and applies it
// without restarting the engine, mirroring POST /config/reload in x/api.
//
// Note that this discards any services added through the Web API: the parser
// only knows about the configuration the host passed in.
func (e *Engine) Reload() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.running {
		return ErrNotRunning
	}

	cfg, err := e.parse(Options{Config: e.config, Debug: e.debug, APIAddr: e.apiAddr})
	if err != nil {
		return err
	}

	config.Set(cfg)

	e.installLogger()

	if err := loader.Load(cfg); err != nil {
		return err
	}

	return e.run(cfg)
}

// SetLogLevel changes the minimum level forwarded to the host callback.
func (e *Engine) SetLogLevel(level Level) { e.emitter.setLevel(level) }

// LogLevel returns the current minimum forwarded level.
func (e *Engine) LogLevel() Level { return e.emitter.level() }

// SetLogSink installs or clears the host callback. It takes effect
// immediately, whether or not the engine is running.
func (e *Engine) SetLogSink(h LogHandler) { e.emitter.setHandler(h) }

// Running reports whether an instance is up.
func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.running
}

// ServiceNames returns the names of the services currently registered. It is
// handy for tests and diagnostics.
func (e *Engine) ServiceNames() []string {
	all := registry.ServiceRegistry().GetAll()
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	return names
}
