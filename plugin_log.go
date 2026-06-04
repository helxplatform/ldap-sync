package main

import (
	"context"
	"database/sql"
	"fmt"

	"gopkg.in/yaml.v2"
)

// logPlugin is the verification plugin: it logs every event it receives. It
// performs no side effects and never fails. Use it to confirm that plugin
// dispatch is firing exactly when expected (created once per new entry,
// updated on actual change, never on unchanged refreshes) before wiring
// real plugins like the PVC plugin.
type logPlugin struct{}

func (logPlugin) Name() string             { return "log" }
func (logPlugin) Match(_ SyncEvent) bool   { return true }
func (logPlugin) Apply(_ context.Context, e SyncEvent) error {
	logger.Info("plugin/log: sync event",
		"DN", e.DN,
		"Op", e.Op,
		"SearchID", e.SearchID,
		"Timestamp", e.Timestamp,
	)
	return nil
}

// buildPlugin is the factory used by initPluginRegistry to construct a Plugin
// from its configured name. New plugins should be added here.
func buildPlugin(cfg PluginConfig) (Plugin, error) {
	switch cfg.Name {
	case "log":
		return logPlugin{}, nil
	case "pvc-group":
		var opts pvcGroupConfig
		if err := decodeOptions(cfg.Options, &opts); err != nil {
			return nil, fmt.Errorf("pvc-group: decode options: %w", err)
		}
		kc, err := buildKubeClient(opts.Kubeconfig)
		if err != nil {
			return nil, err
		}
		return newPVCGroupPlugin(opts, clientGoPVCClient{c: kc})
	case "postgres-user":
		var opts postgresUserConfig
		if err := decodeOptions(cfg.Options, &opts); err != nil {
			return nil, fmt.Errorf("postgres-user: decode options: %w", err)
		}
		opts = opts.withDefaults()
		dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
			opts.Username, opts.Password, opts.Host, opts.Port, opts.Database, opts.SSLMode)
		adminDB, err := sql.Open("postgres", dsn)
		if err != nil {
			return nil, fmt.Errorf("postgres-user: open admin connection: %w", err)
		}
		kc, err := buildKubeClient(opts.Kubeconfig)
		if err != nil {
			return nil, err
		}
		admin := sqlPGAdmin{db: stdSQLAdapter{db: adminDB}}
		return newPostgresUserPlugin(opts, admin, clientGoSecretClient{c: kc})
	default:
		return nil, fmt.Errorf("unknown plugin %q", cfg.Name)
	}
}

// stdSQLAdapter adapts *sql.DB to the sqlExecQuerier surface used by sqlPGAdmin.
type stdSQLAdapter struct {
	db *sql.DB
}

func (a stdSQLAdapter) ExecContext(ctx context.Context, query string, args ...interface{}) (sqlResult, error) {
	return a.db.ExecContext(ctx, query, args...)
}

func (a stdSQLAdapter) QueryRowScanText(ctx context.Context, query string, args ...interface{}) (string, bool, error) {
	var val string
	err := a.db.QueryRowContext(ctx, query, args...).Scan(&val)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

// decodeOptions round-trips the generic options map through YAML so the
// target struct's yaml tags drive the decode. This avoids hand-writing a
// reflect-based decoder and stays consistent with how the rest of Config is
// loaded.
func decodeOptions(in map[string]interface{}, out interface{}) error {
	if in == nil {
		return nil
	}
	raw, err := yaml.Marshal(in)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(raw, out)
}

// initPluginRegistry constructs the package-level pluginRegistry from config.
// Disabled plugins are skipped. Unknown plugin names are logged but do not
// abort startup, so config-only experimentation is safe.
func initPluginRegistry(cfg PluginsConfig) {
	pluginRegistry = NewRegistry(PluginRetry{
		MaxAttempts:    cfg.Retry.MaxAttempts,
		InitialDelayMs: cfg.Retry.InitialDelayMs,
		MaxDelayMs:     cfg.Retry.MaxDelayMs,
	})
	for _, pc := range cfg.Enabled {
		if !pc.Enabled {
			continue
		}
		p, err := buildPlugin(pc)
		if err != nil {
			logger.Warn("Skipping plugin", "Name", pc.Name, "Err", err)
			continue
		}
		pluginRegistry.Register(p)
		logger.Info("Plugin registered", "Name", p.Name())
	}
}
