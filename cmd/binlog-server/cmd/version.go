// Package cmd provides module-level functionality for cmd.
// input: build metadata, root cobra command wiring, process startup dependencies
// output: version banner rendering, version-only output, and default app startup hook
// pos: CLI support layer separating user-facing version queries from runtime boot
// note: if this file changes, update this header and module README.md.
package cmd

import (
	"context"
	"fmt"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"binlog_server/internal/app"
	"binlog_server/internal/config"
	"binlog_server/internal/logging"

	"github.com/spf13/cobra"
)

var (
	buildVersion = ""
	buildCommit  = ""
	buildDate    = ""
)

type versionInfo struct {
	Version string
	Commit  string
	Date    string
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprint(cmd.OutOrStdout(), renderVersionBanner(effectiveVersionInfo()))
			return err
		},
	}
}

func defaultRunRootApp(configPath, encryptionKey string) error {
	cfg, err := config.LoadConfigWithEncryption(configPath, encryptionKey)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	_, cleanupLogger, err := logging.Setup(ctx, cfg.Log)
	if err != nil {
		return fmt.Errorf("setup logger: %w", err)
	}
	defer cleanupLogger()

	if err := app.New(cfg).Run(ctx); err != nil {
		return fmt.Errorf("run app: %w", err)
	}
	return nil
}

func effectiveVersionInfo() versionInfo {
	info := versionInfo{
		Version: "devel",
		Commit:  "unknown",
		Date:    "unknown",
	}

	if strings.TrimSpace(buildVersion) != "" {
		info.Version = buildVersion
	}
	if strings.TrimSpace(buildCommit) != "" {
		info.Commit = buildCommit
	}
	if strings.TrimSpace(buildDate) != "" {
		info.Date = buildDate
	}

	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		if info.Version == "devel" && strings.TrimSpace(buildInfo.Main.Version) != "" && buildInfo.Main.Version != "(devel)" {
			info.Version = buildInfo.Main.Version
		}
		for _, setting := range buildInfo.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "unknown" && strings.TrimSpace(setting.Value) != "" {
					info.Commit = setting.Value
				}
			case "vcs.time":
				if info.Date == "unknown" && strings.TrimSpace(setting.Value) != "" {
					info.Date = setting.Value
				}
			}
		}
	}

	return info
}

func renderVersionBanner(info versionInfo) string {
	lines := []string{
		` ____                  ___                       ____`,
		`/\  _` + "`" + `\    __         /\_ \                     /\  _` + "`" + `\`,
		`\ \ \L\ \ /\_\    ___ \//\ \     ___      __    \ \,\L\_\     __   _ __   __  __     __   _ __`,
		` \ \  _ <'\/\ \ /' _ ` + "`" + `\ \ \ \   / __` + "`" + `\  /'_ ` + "`" + `\   \/_\__ \   /'__` + "`" + `\/\` + "`" + `'__\/\ \/\ \  /'__` + "`" + `\/\` + "`" + `'__\`,
		`  \ \ \L\ \\ \ \/\ \/\ \ \_\ \_/\ \L\ \/\ \L\ \    /\ \L\ \/\  __/\ \ \/ \ \ \_/ |/\  __/\ \ \/`,
		`   \ \____/ \ \_\ \_\ \_\/\____\ \____/\ \____ \   \ ` + "`" + `\____\ \____\\ \_\  \ \___/ \ \____\\ \_\`,
		`    \/___/   \/_/\/_/\/_/\/____/\/___/  \/___L\ \   \/_____/\/____/ \/_/   \/__/   \/____/ \/_/`,
		`                                          /\____/`,
		`                                          \_/__/`,
		"",
		"BinlogServer",
		fmt.Sprintf("Version: %s", info.Version),
		fmt.Sprintf("Commit:  %s", info.Commit),
		fmt.Sprintf("Built:   %s", info.Date),
	}
	return strings.Join(lines, "\n") + "\n"
}
