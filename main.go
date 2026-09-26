// Package main is the entry point for the 3AX-UI web panel application.
// It initializes the database, web server, and handles command-line operations for managing the panel.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	_ "unsafe"

	"github.com/coinman-dev/3ax-ui/v2/config"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/nginx"
	"github.com/coinman-dev/3ax-ui/v2/proxy"
	"github.com/coinman-dev/3ax-ui/v2/sub"
	"github.com/coinman-dev/3ax-ui/v2/tunnel"
	"github.com/coinman-dev/3ax-ui/v2/util/crypto"
	"github.com/coinman-dev/3ax-ui/v2/util/sys"
	"github.com/coinman-dev/3ax-ui/v2/web"
	"github.com/coinman-dev/3ax-ui/v2/web/global"
	"github.com/coinman-dev/3ax-ui/v2/web/service"

	"github.com/joho/godotenv"
	"github.com/op/go-logging"
)

// runWebServer initializes and starts the web server for the 3AX-UI panel.
func runWebServer() {
	log.Printf("Starting %v %v", config.GetName(), config.GetVersion())

	switch config.GetLogLevel() {
	case config.Debug:
		logger.InitLogger(logging.DEBUG)
	case config.Info:
		logger.InitLogger(logging.INFO)
	case config.Notice:
		logger.InitLogger(logging.NOTICE)
	case config.Warning:
		logger.InitLogger(logging.WARNING)
	case config.Error:
		logger.InitLogger(logging.ERROR)
	default:
		log.Fatalf("Unknown log level: %v", config.GetLogLevel())
	}

	godotenv.Load()

	err := database.InitDB(config.GetDBPath())
	if err != nil {
		log.Fatalf("Error initializing database: %v", err)
	}

	var server *web.Server
	server = web.NewServer()
	global.SetWebServer(server)
	err = server.Start()
	if err != nil {
		log.Fatalf("Error starting web server: %v", err)
		return
	}

	var subServer *sub.Server
	subServer = sub.NewServer()
	err = subServer.Start()
	if err != nil {
		log.Fatalf("Error starting sub server: %v", err)
		return
	}

	sigCh := make(chan os.Signal, 1)
	// Trap shutdown signals
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM, sys.SIGUSR1)
	for {
		sig := <-sigCh

		switch sig {
		case syscall.SIGHUP:
			logger.Info("Received SIGHUP signal. Restarting servers...")

			// --- FIX FOR TELEGRAM BOT CONFLICT (409): Stop bot before restart ---
			service.StopBot()
			// --

			err := server.Stop()
			if err != nil {
				logger.Debug("Error stopping web server:", err)
			}
			err = subServer.Stop()
			if err != nil {
				logger.Debug("Error stopping sub server:", err)
			}

			server = web.NewServer()
			global.SetWebServer(server)
			err = server.Start()
			if err != nil {
				log.Fatalf("Error restarting web server: %v", err)
				return
			}
			log.Println("Web server restarted successfully.")

			subServer = sub.NewServer()
			err = subServer.Start()
			if err != nil {
				log.Fatalf("Error restarting sub server: %v", err)
				return
			}
			log.Println("Sub server restarted successfully.")
		case sys.SIGUSR1:
			logger.Info("Received USR1 signal, restarting xray-core...")
			err := server.RestartXray()
			if err != nil {
				logger.Error("Failed to restart xray-core:", err)
			}

		default:
			// --- FIX FOR TELEGRAM BOT CONFLICT (409) on full shutdown ---
			service.StopBot()
			// ------------------------------------------------------------

			server.Stop()
			subServer.Stop()
			logger.CloseLogger()
			log.Println("Shutting down servers.")
			return
		}
	}
}

// resetSetting resets all panel settings to their default values.
func resetSetting() error {
	err := database.InitDB(config.GetDBPath())
	if err != nil {
		fmt.Println("Failed to initialize database:", err)
		return err
	}

	settingService := service.SettingService{}
	err = settingService.ResetSettings()
	if err != nil {
		fmt.Println("Failed to reset settings:", err)
		return err
	} else {
		fmt.Println("Settings successfully reset.")
	}
	return nil
}

// showSetting displays the current panel settings if show is true.
func showSetting(show bool) {
	if show {
		settingService := service.SettingService{}
		port, err := settingService.GetPort()
		if err != nil {
			fmt.Println("get current port failed, error info:", err)
		}

		webBasePath, err := settingService.GetBasePath()
		if err != nil {
			fmt.Println("get webBasePath failed, error info:", err)
		}

		certFile, err := settingService.GetCertFile()
		if err != nil {
			fmt.Println("get cert file failed, error info:", err)
		}
		keyFile, err := settingService.GetKeyFile()
		if err != nil {
			fmt.Println("get key file failed, error info:", err)
		}

		userService := service.UserService{}
		userModel, err := userService.GetFirstUser()
		if err != nil {
			fmt.Println("get current user info failed, error info:", err)
		}

		if userModel.Username == "" || userModel.Password == "" {
			fmt.Println("current username or password is empty")
		}

		fmt.Println("current panel settings as follows:")
		if certFile == "" || keyFile == "" {
			fmt.Println("Warning: Panel is not secure with SSL")
		} else {
			fmt.Println("Panel is secure with SSL")
		}

		hasDefaultCredential := func() bool {
			return userModel.Username == "admin" && crypto.CheckPasswordHash(userModel.Password, "admin")
		}()

		fmt.Println("hasDefaultCredential:", hasDefaultCredential)
		fmt.Println("port:", port)
		fmt.Println("webBasePath:", webBasePath)
	}
}

// updateTgbotEnableSts enables or disables the Telegram bot notifications based on the status parameter.
func updateTgbotEnableSts(status bool) {
	settingService := service.SettingService{}
	currentTgSts, err := settingService.GetTgbotEnabled()
	if err != nil {
		fmt.Println(err)
		return
	}
	logger.Infof("current enabletgbot status[%v],need update to status[%v]", currentTgSts, status)
	if currentTgSts != status {
		err := settingService.SetTgbotEnabled(status)
		if err != nil {
			fmt.Println(err)
			return
		} else {
			logger.Infof("SetTgbotEnabled[%v] success", status)
		}
	}
}

// updateTgbotSetting updates Telegram bot settings including token, chat ID, and runtime schedule.
func updateTgbotSetting(tgBotToken string, tgBotChatid string, tgBotRuntime string) {
	err := database.InitDB(config.GetDBPath())
	if err != nil {
		fmt.Println("Error initializing database:", err)
		return
	}

	settingService := service.SettingService{}

	if tgBotToken != "" {
		err := settingService.SetTgBotToken(tgBotToken)
		if err != nil {
			fmt.Printf("Error setting Telegram bot token: %v\n", err)
			return
		}
		logger.Info("Successfully updated Telegram bot token.")
	}

	if tgBotRuntime != "" {
		err := settingService.SetTgbotRuntime(tgBotRuntime)
		if err != nil {
			fmt.Printf("Error setting Telegram bot runtime: %v\n", err)
			return
		}
		logger.Infof("Successfully updated Telegram bot runtime to [%s].", tgBotRuntime)
	}

	if tgBotChatid != "" {
		err := settingService.SetTgBotChatId(tgBotChatid)
		if err != nil {
			fmt.Printf("Error setting Telegram bot chat ID: %v\n", err)
			return
		}
		logger.Info("Successfully updated Telegram bot chat ID.")
	}
}

// showMonSetting prints the monitoring state and bearer token the mon-server
// must present (docs/spec/monitoring-panel.md §7.3) — the CLI duplicate of the
// Monitoring settings tab, for an operator who only has a shell. The database
// must already be initialised; the caller does that, as it does for the tgbot
// flags, so tests can point these helpers at a temporary database.
func showMonSetting(w io.Writer) error {
	settingService := service.SettingService{}
	enable, err := settingService.GetMonEnable()
	if err != nil {
		return fmt.Errorf("failed to read monEnable: %w", err)
	}
	token, err := settingService.GetMonToken()
	if err != nil {
		return fmt.Errorf("failed to read monToken: %w", err)
	}
	fmt.Fprintf(w, "monEnable: %v\n", enable)
	if token == "" {
		// An empty token keeps /mon/v1 closed however monEnable is set, so say
		// so rather than printing a blank value.
		fmt.Fprintln(w, "monToken: (not issued)")
	} else {
		fmt.Fprintf(w, "monToken: %s\n", token)
	}
	return nil
}

// resetMonToken issues a fresh monitoring token and prints the resulting state.
// Whatever the mon-server is using stops working the moment this returns.
func resetMonToken(w io.Writer) error {
	settingService := service.SettingService{}
	if _, err := settingService.ResetMonToken(); err != nil {
		return fmt.Errorf("failed to reset monToken: %w", err)
	}
	return showMonSetting(w)
}

// setMonEnable opens or closes the /mon/v1 endpoints. raw is the flag value as
// typed; anything strconv.ParseBool refuses leaves the setting untouched.
func setMonEnable(w io.Writer, raw string) error {
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fmt.Errorf("invalid -monEnable value %q: expected true or false", raw)
	}
	settingService := service.SettingService{}
	if err := settingService.SetMonEnable(value); err != nil {
		return fmt.Errorf("failed to set monEnable: %w", err)
	}
	fmt.Fprintf(w, "monEnable: %v\n", value)
	return nil
}

// runMonSetting applies the monitoring flags of the `setting` subcommand in the
// order enable → reset → show, printing the final state exactly once: when a
// reset or a show follows, the enable step's own line is dropped rather than
// printed above the same line again.
func runMonSetting(w io.Writer, monEnableRaw string, reset bool, show bool) error {
	if monEnableRaw != "" {
		out := w
		if reset || show {
			out = io.Discard
		}
		if err := setMonEnable(out, monEnableRaw); err != nil {
			return err
		}
	}
	if reset {
		return resetMonToken(w)
	}
	if show {
		return showMonSetting(w)
	}
	return nil
}

// updateSetting updates various panel settings including port, credentials, base path, listen IP, and two-factor authentication.
func updateSetting(port int, username string, password string, webBasePath string, listenIP string, resetTwoFactor bool) error {
	err := database.InitDB(config.GetDBPath())
	if err != nil {
		fmt.Println("Database initialization failed:", err)
		return err
	}

	settingService := service.SettingService{}
	userService := service.UserService{}

	if port > 0 {
		err := settingService.SetPort(port)
		if err != nil {
			fmt.Println("Failed to set port:", err)
		} else {
			fmt.Printf("Port set successfully: %v\n", port)
		}
	}

	if username != "" || password != "" {
		err := userService.UpdateFirstUser(username, password)
		if err != nil {
			fmt.Println("Failed to update username and password:", err)
		} else {
			fmt.Println("Username and password updated successfully")
		}
	}

	if webBasePath != "" {
		err := settingService.SetBasePath(webBasePath)
		if err != nil {
			fmt.Println("Failed to set base URI path:", err)
		} else {
			fmt.Println("Base URI path set successfully")
		}
	}

	if resetTwoFactor {
		err := settingService.SetTwoFactorEnable(false)

		if err != nil {
			fmt.Println("Failed to reset two-factor authentication:", err)
		} else {
			settingService.SetTwoFactorToken("")
			fmt.Println("Two-factor authentication reset successfully")
		}
	}

	if listenIP != "" {
		err := settingService.SetListen(listenIP)
		if err != nil {
			fmt.Println("Failed to set listen IP:", err)
		} else {
			fmt.Printf("listen %v set successfully", listenIP)
		}
	}

	return nil
}

// updateCert updates the SSL certificate files for the panel.
func updateCert(publicKey string, privateKey string) {
	err := database.InitDB(config.GetDBPath())
	if err != nil {
		fmt.Println(err)
		return
	}

	if (privateKey != "" && publicKey != "") || (privateKey == "" && publicKey == "") {
		settingService := service.SettingService{}
		err = settingService.SetCertFile(publicKey)
		if err != nil {
			fmt.Println("set certificate public key failed:", err)
		} else {
			fmt.Println("set certificate public key success")
		}

		err = settingService.SetKeyFile(privateKey)
		if err != nil {
			fmt.Println("set certificate private key failed:", err)
		} else {
			fmt.Println("set certificate private key success")
		}

		err = settingService.SetSubCertFile(publicKey)
		if err != nil {
			fmt.Println("set certificate for subscription public key failed:", err)
		} else {
			fmt.Println("set certificate for subscription public key success")
		}

		err = settingService.SetSubKeyFile(privateKey)
		if err != nil {
			fmt.Println("set certificate for subscription private key failed:", err)
		} else {
			fmt.Println("set certificate for subscription private key success")
		}
	} else {
		fmt.Println("both public and private key should be entered.")
	}
}

// GetCertificate displays the current SSL certificate settings if getCert is true.
func GetCertificate(getCert bool) {
	if getCert {
		settingService := service.SettingService{}
		certFile, err := settingService.GetCertFile()
		if err != nil {
			fmt.Println("get cert file failed, error info:", err)
		}
		keyFile, err := settingService.GetKeyFile()
		if err != nil {
			fmt.Println("get key file failed, error info:", err)
		}

		fmt.Println("cert:", certFile)
		fmt.Println("key:", keyFile)
	}
}

// GetListenIP displays the current panel listen IP address if getListen is true.
func GetListenIP(getListen bool) {
	if getListen {

		settingService := service.SettingService{}
		ListenIP, err := settingService.GetListen()
		if err != nil {
			log.Printf("Failed to retrieve listen IP: %v", err)
			return
		}

		fmt.Println("listenIP:", ListenIP)
	}
}

// migrateDb performs database migration operations for the 3AX-UI panel.
func migrateDb() {
	inboundService := service.InboundService{}

	err := database.InitDB(config.GetDBPath())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Start migrating database...")
	inboundService.MigrateDB()
	fmt.Println("Migration done!")
}

// chainPorts is the debug export of the relayed ports the chain document
// carries (docs/spec/proxy-chain.md §5.8, §3.8). It replaces `x-ui
// relay-manifest`: nothing is pasted anywhere any more, the fronts get this
// same list through the wave, and printing it is how an operator checks what
// the panel believes it publishes.
func chainPorts(out string) {
	if err := database.InitDB(config.GetDBPath()); err != nil {
		log.Fatalf("chain ports: %v", err)
	}
	ports, err := (&service.ChainPortsService{}).Ports()
	if err != nil {
		log.Fatalf("chain ports: %v", err)
	}
	data, err := json.MarshalIndent(ports, "", "  ")
	if err != nil {
		log.Fatalf("chain ports: %v", err)
	}
	data = append(data, '\n')
	if out == "" {
		os.Stdout.Write(data)
		return
	}
	if err := os.WriteFile(out, data, 0o600); err != nil {
		log.Fatalf("chain ports: %v", err)
	}
	fmt.Printf("chain ports written to %s\n", out)
}

// defaultProxyConfig is where a box keeps its proxy.json (§5.5).
const defaultProxyConfig = "/etc/x-ui/proxy.json"

// chainCommand runs `x-ui chain <subcommand>` and returns the process exit
// code. One subcommand belongs to the panel (`ports`), three to a box
// (`join-url`, `status`, `rejoin`) — the same binary runs in both roles, so
// they live in one place (docs/spec/proxy-chain.md §5.8).
func chainCommand(args []string, out io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(out, "chain: subcommands are `ports` (panel), `join-url`, `status` and `rejoin` (box)")
		return 1
	}
	switch args[0] {
	case "ports":
		cmd := flag.NewFlagSet("chain ports", flag.ContinueOnError)
		cmd.SetOutput(out)
		portsOut := cmd.String("o", "", "write the port list to this file instead of stdout")
		if err := cmd.Parse(args[1:]); err != nil {
			return 1
		}
		chainPorts(*portsOut)
		return 0

	case "join-url":
		cmd := flag.NewFlagSet("chain join-url", flag.ContinueOnError)
		cmd.SetOutput(out)
		cfgPath := cmd.String("c", defaultProxyConfig, "path to the proxy-front config JSON")
		if err := cmd.Parse(args[1:]); err != nil {
			return 1
		}
		url, err := proxy.ReadJoinURL(*cfgPath)
		if err != nil {
			fmt.Fprintln(out, err)
			return 1
		}
		fmt.Fprintln(out, url)
		return 0

	case "status":
		cmd := flag.NewFlagSet("chain status", flag.ContinueOnError)
		cmd.SetOutput(out)
		cfgPath := cmd.String("c", defaultProxyConfig, "path to the proxy-front config JSON")
		if err := cmd.Parse(args[1:]); err != nil {
			return 1
		}
		cfg, err := proxy.LoadConfig(*cfgPath)
		if err != nil {
			fmt.Fprintln(out, err)
			return 1
		}
		status, err := proxy.FetchStatus(context.Background(), cfg)
		if err != nil {
			fmt.Fprintln(out, err)
			return 1
		}
		proxy.PrintStatus(out, status)
		return 0

	case "rejoin":
		cmd := flag.NewFlagSet("chain rejoin", flag.ContinueOnError)
		cmd.SetOutput(out)
		cfgPath := cmd.String("c", defaultProxyConfig, "path to the proxy-front config JSON")
		nextHop := cmd.String("next-hop", "", "address of the next hop (the panel, for the innermost hop)")
		subPort := cmd.Int("sub-port", 0, "sub port of the next hop (default: keep the configured one, else 2096)")
		scheme := cmd.String("scheme", "", "http|https of the next hop's sub port (default: keep the configured one)")
		token := cmd.String("token", "", "a fresh join token from the panel's chain registry")
		if err := cmd.Parse(args[1:]); err != nil {
			return 1
		}
		cfg, err := proxy.LoadConfig(*cfgPath)
		if err != nil {
			fmt.Fprintln(out, err)
			return 1
		}
		result, err := proxy.Rejoin(context.Background(), cfg, *nextHop, *subPort, *scheme, *token)
		if err != nil {
			fmt.Fprintln(out, err)
			return 1
		}
		fmt.Fprintf(out, "joined the chain as %q (%s) at revision %d, next hop %s\n",
			result.Document.Self.Name, result.Document.Self.Role, result.Document.Revision, cfg.NextHopBase())
		fmt.Fprintln(out, "restart x-ui to apply (systemctl restart x-ui)")
		return 0

	default:
		fmt.Fprintf(out, "chain: unknown subcommand %q; try `ports`, `join-url`, `status` or `rejoin`\n", args[0])
		return 1
	}
}

// nginxCommand runs `x-ui nginx <subcommand>` and returns the process exit
// code. The same binary runs on the panel and on every hop, and both need port
// 80 answered by nginx before acme.sh can issue or renew a certificate there.
func nginxCommand(args []string, out io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(out, "nginx: subcommands are `acme-front` (serve the ACME webroot on port 80)")
		return 1
	}
	switch args[0] {
	case "acme-front":
		cmd := flag.NewFlagSet("nginx acme-front", flag.ContinueOnError)
		cmd.SetOutput(out)
		if err := cmd.Parse(args[1:]); err != nil {
			return 1
		}
		changed, err := nginx.EnsureACMEFront()
		if err != nil {
			fmt.Fprintf(out, "nginx acme-front: %v\n", err)
			return 1
		}
		if changed {
			fmt.Fprintf(out, "nginx now answers ACME challenges on port 80 from %s\n", nginx.ACMEWebroot)
		} else {
			fmt.Fprintf(out, "nginx already answers ACME challenges on port 80 from %s\n", nginx.ACMEWebroot)
		}
		return 0
	default:
		fmt.Fprintf(out, "nginx: unknown subcommand %q; try `acme-front`\n", args[0])
		return 1
	}
}

// generateAwg2 fills the AmneziaWG server row with freshly generated 2.0
// obfuscation parameters (DB only, no interface changes). Used by install.sh on
// a FRESH install so new setups default to AmneziaWG 2.0; on update the caller
// skips this so existing servers keep their params (admin opts in via the panel).
func generateAwg2() {
	if err := database.InitDB(config.GetDBPath()); err != nil {
		log.Fatal(err)
	}
	db := database.GetDB()
	var server model.TunnelServer
	if err := db.Where("kind = ?", model.TunnelKindAwg).First(&server).Error; err != nil {
		fmt.Println("No AmneziaWG server to configure:", err)
		return
	}
	o := tunnel.GenerateObfuscation20("default")
	prevS4 := server.S4
	server.Jc, server.Jmin, server.Jmax = o.Jc, o.Jmin, o.Jmax
	server.S1, server.S2, server.S3, server.S4 = o.S1, o.S2, o.S3, o.S4
	server.H1, server.H2, server.H3, server.H4 = o.H1, o.H2, o.H3, o.H4
	server.I1 = o.I1
	// install.sh wrote the legacy 1420, which the new S4 padding overflows.
	tunnel.FollowServerMTU(tunnel.AWG, &server, prevS4)
	if err := db.Save(&server).Error; err != nil {
		fmt.Println("Failed to save AmneziaWG 2.0 parameters:", err)
		return
	}
	fmt.Println("AmneziaWG 2.0 obfuscation parameters generated.")
}

// main is the entry point of the 3AX-UI application.
// It parses command-line arguments to run the web server, migrate database, or update settings.
func main() {
	if len(os.Args) < 2 {
		runWebServer()
		return
	}

	var showVersion bool
	flag.BoolVar(&showVersion, "v", false, "show version")

	runCmd := flag.NewFlagSet("run", flag.ExitOnError)

	settingCmd := flag.NewFlagSet("setting", flag.ExitOnError)
	var port int
	var username string
	var password string
	var webBasePath string
	var listenIP string
	var getListen bool
	var webCertFile string
	var webKeyFile string
	var tgbottoken string
	var tgbotchatid string
	var enabletgbot bool
	var tgbotRuntime string
	var reset bool
	var show bool
	var getCert bool
	var resetTwoFactor bool
	var showMonToken bool
	var resetMonTokenFlag bool
	var monEnableRaw string
	settingCmd.BoolVar(&reset, "reset", false, "Reset all settings")
	settingCmd.BoolVar(&show, "show", false, "Display current settings")
	settingCmd.IntVar(&port, "port", 0, "Set panel port number")
	settingCmd.StringVar(&username, "username", "", "Set login username")
	settingCmd.StringVar(&password, "password", "", "Set login password")
	settingCmd.StringVar(&webBasePath, "webBasePath", "", "Set base path for Panel")
	settingCmd.StringVar(&listenIP, "listenIP", "", "set panel listenIP IP")
	settingCmd.BoolVar(&resetTwoFactor, "resetTwoFactor", false, "Reset two-factor authentication settings")
	settingCmd.BoolVar(&getListen, "getListen", false, "Display current panel listenIP IP")
	settingCmd.BoolVar(&getCert, "getCert", false, "Display current certificate settings")
	settingCmd.StringVar(&webCertFile, "webCert", "", "Set path to public key file for panel")
	settingCmd.StringVar(&webKeyFile, "webCertKey", "", "Set path to private key file for panel")
	settingCmd.StringVar(&tgbottoken, "tgbottoken", "", "Set token for Telegram bot")
	settingCmd.StringVar(&tgbotRuntime, "tgbotRuntime", "", "Set cron time for Telegram bot notifications")
	settingCmd.StringVar(&tgbotchatid, "tgbotchatid", "", "Set chat ID for Telegram bot notifications")
	settingCmd.BoolVar(&enabletgbot, "enabletgbot", false, "Enable notifications via Telegram bot")
	settingCmd.BoolVar(&showMonToken, "showMonToken", false, "Display the monitoring state and the mon-server bearer token")
	settingCmd.BoolVar(&resetMonTokenFlag, "resetMonToken", false, "Issue a new mon-server bearer token (the old one stops working)")
	settingCmd.StringVar(&monEnableRaw, "monEnable", "", "Open or close the /mon/v1 endpoints (true|false)")

	oldUsage := flag.Usage
	flag.Usage = func() {
		oldUsage()
		fmt.Println()
		fmt.Println("Commands:")
		fmt.Println("    run            run web panel")
		fmt.Println("    migrate        migrate form other/old x-ui")
		fmt.Println("    setting        set settings")
		fmt.Println("    proxy          run one hop of the proxy chain (dokodemo relay + sub port)")
		fmt.Println("    chain ports    print the relayed ports of the chain document (debug export, panel)")
		fmt.Println("    chain join-url show the pending join-page link of a box (box)")
		fmt.Println("    chain status   show this box's place in the chain (box)")
		fmt.Println("    chain rejoin   point this box at a next hop and join it with a fresh token (box)")
		fmt.Println("    nginx acme-front  let nginx answer ACME challenges on port 80 (panel and box)")
	}

	flag.Parse()
	if showVersion {
		fmt.Println(config.GetVersion())
		return
	}

	switch os.Args[1] {
	case "run":
		err := runCmd.Parse(os.Args[2:])
		if err != nil {
			fmt.Println(err)
			return
		}
		runWebServer()
	case "migrate":
		migrateDb()
	case "awg-gen2":
		generateAwg2()
	case "setting":
		err := settingCmd.Parse(os.Args[2:])
		if err != nil {
			fmt.Println(err)
			return
		}
		if reset {
			if err = resetSetting(); err != nil {
				return
			}
		} else {
			if err = updateSetting(port, username, password, webBasePath, listenIP, resetTwoFactor); err != nil {
				return
			}
		}
		if show {
			showSetting(show)
		}
		if getListen {
			GetListenIP(getListen)
		}
		if getCert {
			GetCertificate(getCert)
		}
		if (tgbottoken != "") || (tgbotchatid != "") || (tgbotRuntime != "") {
			updateTgbotSetting(tgbottoken, tgbotchatid, tgbotRuntime)
		}
		if enabletgbot {
			updateTgbotEnableSts(enabletgbot)
		}
		// The database is already initialised above, by resetSetting or
		// updateSetting, as it is for the tgbot flags.
		if err = runMonSetting(os.Stdout, monEnableRaw, resetMonTokenFlag, showMonToken); err != nil {
			// A refused -monEnable value must not look like success to x-ui.sh
			// or to any other script driving the CLI.
			fmt.Println(err)
			os.Exit(1)
		}
	case "cert":
		err := settingCmd.Parse(os.Args[2:])
		if err != nil {
			fmt.Println(err)
			return
		}
		if reset {
			updateCert("", "")
		} else {
			updateCert(webCertFile, webKeyFile)
		}
	case "chain":
		os.Exit(chainCommand(os.Args[2:], os.Stdout))
	case "nginx":
		os.Exit(nginxCommand(os.Args[2:], os.Stdout))
	case "proxy":
		proxyCmd := flag.NewFlagSet("proxy", flag.ExitOnError)
		var proxyConfigPath string
		proxyCmd.StringVar(&proxyConfigPath, "c", "", "path to the proxy-front config JSON")
		if err := proxyCmd.Parse(os.Args[2:]); err != nil {
			fmt.Println(err)
			return
		}
		if proxyConfigPath == "" {
			fmt.Println("proxy: -c <config.json> is required")
			return
		}
		cfg, err := proxy.LoadConfig(proxyConfigPath)
		if err != nil {
			log.Fatalf("proxy: %v", err)
		}
		// The box's front serves the panel's own built-in cover page as its
		// decoy unless proxy.json names another (#140).
		stub := ""
		if tpl, ok := (&service.StubService{}).DefaultTemplate(); ok {
			stub = tpl.Html
		}
		if err := proxy.Run(cfg, stub); err != nil {
			log.Fatalf("proxy: %v", err)
		}
	default:
		fmt.Println("Invalid subcommands")
		fmt.Println()
		runCmd.Usage()
		fmt.Println()
		settingCmd.Usage()
	}
}
