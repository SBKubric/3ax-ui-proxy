package database

import xuilogger "github.com/coinman-dev/3ax-ui/v2/logger"

// Post-migration hooks are one-time migrations that need a service the
// database package cannot call directly: web/service imports database, so the
// reverse import would be a cycle. A service registers its hook from an init
// function, and InitDB runs the registered hooks once the schema is in place.
//
// The chain registry uses this for MigrateLegacyOverride (docs/spec/proxy-chain.md
// §2.3), which imports a hand-set proxyOverrideHost into the registry.
var postMigrateHooks []func() error

// RegisterPostMigrate adds a hook to run at the end of InitDB's migrations.
// Call it from an init function, so registration is finished before any
// InitDB.
func RegisterPostMigrate(hook func() error) {
	if hook == nil {
		return
	}
	postMigrateHooks = append(postMigrateHooks, hook)
}

// runPostMigrateHooks runs every registered hook. A failing hook is logged and
// the rest still run: a migration that could not import an old setting must
// not keep the panel from starting.
func runPostMigrateHooks() {
	for _, hook := range postMigrateHooks {
		if err := hook(); err != nil {
			xuilogger.Warning("post-migration hook failed:", err)
		}
	}
}
