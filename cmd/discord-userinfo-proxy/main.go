// Command discord-userinfo-proxy adds Discord guild roles to a Dex oauth connector's
// userinfo response and rejects users who are not in the guild.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/xonvanetta/discord-userinfo-proxy/internal/discord"
	"github.com/xonvanetta/discord-userinfo-proxy/internal/groups"
	"github.com/xonvanetta/discord-userinfo-proxy/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// A flag left unset falls back to the environment variable of the same name in
	// upper case with dashes as underscores, so --discord-bot-token is DISCORD_BOT_TOKEN.
	// The environment keeps the bot token out of the process list.
	showVersion := pflag.Bool("version", false, "print the version and exit")
	pflag.String("listen-addr", ":8080", "address to listen on")
	pflag.String("discord-bot-token", "", "Discord bot token, without the Bot prefix (required)")
	pflag.String("discord-guild-id", "", "numeric ID of the one guild whose members may log in (required)")
	pflag.Duration("discord-role-cache-ttl", 5*time.Minute, "how long the guild role list is cached; must be positive")
	pflag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	viper.BindPFlags(pflag.CommandLine)
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	config := config{
		ListenAddr: viper.GetString("listen-addr"),
		Discord: discord.Config{
			BotToken: viper.GetString("discord-bot-token"),
			GuildID:  viper.GetString("discord-guild-id"),
		},
		RoleCacheTTL: viper.GetDuration("discord-role-cache-ttl"),
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	err := run(log, config)
	if err != nil {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

// config is the whole runtime configuration.
type config struct {
	ListenAddr   string
	Discord      discord.Config
	RoleCacheTTL time.Duration
}

func run(log *slog.Logger, config config) error {
	userAgent := fmt.Sprintf("DiscordBot (https://github.com/xonvanetta/discord-userinfo-proxy, %s)", version)
	client, err := discord.New(config.Discord, userAgent, &http.Client{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("discord client: %w", err)
	}

	resolver, err := groups.New(client, config.Discord.GuildID, config.RoleCacheTTL)
	if err != nil {
		return fmt.Errorf("groups: %w", err)
	}

	handler := server.New(client, resolver, log)

	srv := &http.Server{
		Addr:              config.ListenAddr,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", config.ListenAddr, "version", version, "guild_id", config.Discord.GuildID)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err = <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
}
