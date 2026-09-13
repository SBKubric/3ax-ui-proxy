package main

import (
	"fmt"

	"github.com/coinman-dev/3ax-ui/v2/config"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
)

// Monitoring settings from the CLI (docs/spec/monitoring-panel.md §7.3, §10):
// the same token and switch the Settings → Monitoring tab manages, for a
// server without a browser at hand. `x-ui setting -showMonToken` prints the
// token, `-resetMonToken` mints a new one (the old one stops working at
// once), `-enableMonitoring` / `-disableMonitoring` flip the switch.

// updateMonitoringSetting applies the monitoring flags of `x-ui setting` and
// prints the resulting state.
func updateMonitoringSetting(showToken, resetToken, enable, disable bool) error {
	if !showToken && !resetToken && !enable && !disable {
		return nil
	}
	if err := database.InitDB(config.GetDBPath()); err != nil {
		fmt.Println("Error initializing database:", err)
		return err
	}
	settingService := service.SettingService{}
	if resetToken {
		token, err := settingService.RegenerateMonToken()
		if err != nil {
			fmt.Println("Failed to reset the monitoring token:", err)
			return err
		}
		fmt.Println("Monitoring token reset; the previous token no longer works. Update the mon-server config.")
		fmt.Println("monToken:", token)
	}
	if enable || disable {
		if err := settingService.SetMonEnable(enable); err != nil {
			fmt.Println("Failed to update monEnable:", err)
			return err
		}
	}
	enabled, err := settingService.GetMonEnable()
	if err != nil {
		fmt.Println("Failed to read monEnable:", err)
		return err
	}
	fmt.Println("monEnable:", enabled)
	if showToken {
		token, err := settingService.GetMonToken()
		if err != nil {
			fmt.Println("Failed to read the monitoring token:", err)
			return err
		}
		if token == "" {
			fmt.Println("monToken: (not set — run `x-ui setting -resetMonToken` to create one)")
		} else {
			fmt.Println("monToken:", token)
		}
	}
	return nil
}
