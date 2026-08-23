package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	discordsignup "github.com/kayushkin/discord-signup-store"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	// Loopback by default, deliberately. The admin routes on this server edit
	// rosters and have no auth of their own; nginx publishes only /interactions.
	// A wildcard bind here would put the whole API on the network.
	addr := envOr("DISCORD_SIGNUP_ADDR", "127.0.0.1:8312")
	dataDir := os.Getenv("DISCORD_SIGNUP_DATA_DIR")

	// No fallback and no default. Without the right public key every request
	// from Discord fails verification, the endpoint will not even save in the
	// Developer Portal, and a service that started anyway would look healthy
	// while answering 401 to everything.
	publicKey := os.Getenv("DISCORD_APPLICATION_PUBLIC_KEY")
	if publicKey == "" {
		log.Fatal("DISCORD_APPLICATION_PUBLIC_KEY is not set — " +
			"copy it from the Discord Developer Portal, General Information")
	}
	verifier, err := discordsignup.NewInteractionVerifier(publicKey)
	if err != nil {
		log.Fatalf("discord public key: %v", err)
	}

	store, err := discordsignup.Open(dataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// The bot token is resolved from auth-store on first use, not read here, so
	// a token rotation is picked up by a 401 retry rather than a restart.
	discord := discordsignup.NewDiscordClient(
		envOr("DISCORD_API_BASE", discordsignup.DiscordAPIBase),
		discordsignup.AuthStoreTokenResolver(
			envOr("AUTH_STORE_URL", "http://127.0.0.1:8303"),
			os.Getenv("AUTH_STORE_TOKEN"),
			envOr("DISCORD_CREDENTIAL_PROVIDER", "discord"),
			envOr("DISCORD_CREDENTIAL_ACCOUNT", "default"),
		),
	)

	srvAPI := discordsignup.NewServer(store, verifier, discord)

	// The zone a time typed into a Discord form is read in. One per deployment
	// because a modal holds five fields and a timezone picker is not worth one
	// of them; the form's own label prints it so nobody has to guess.
	if zone := os.Getenv("DISCORD_DEFAULT_TIMEZONE"); zone != "" {
		if _, err := time.LoadLocation(zone); err != nil {
			log.Fatalf("DISCORD_DEFAULT_TIMEZONE=%q is not an IANA zone name: %v", zone, err)
		}
		srvAPI.SetDefaultTimezone(zone)
	} else {
		log.Print("DISCORD_DEFAULT_TIMEZONE is not set — times typed into Discord forms will " +
			"be read as UTC, which is almost certainly not what anyone means")
	}

	// The browser surface is optional. Without a redirect URL the login routes
	// answer 501 and everything else — buttons, rosters, the API — still works.
	if redirect := os.Getenv("DISCORD_OAUTH_REDIRECT_URL"); redirect != "" {
		srvAPI.EnableWeb(&discordsignup.OAuthConfig{
			// The application id doubles as the OAuth client id.
			ClientID:    envOr("DISCORD_APPLICATION_ID", ""),
			RedirectURL: redirect,
			// A DIFFERENT auth-store credential from the bot token. Two
			// secrets with different blast radii should not share a row: the
			// client secret can mint user logins, the bot token can act in
			// every server.
			ResolveClientSecret: discordsignup.AuthStoreTokenResolver(
				envOr("AUTH_STORE_URL", "http://127.0.0.1:8303"),
				os.Getenv("AUTH_STORE_TOKEN"),
				envOr("DISCORD_CREDENTIAL_PROVIDER", "discord"),
				envOr("DISCORD_OAUTH_CREDENTIAL_ACCOUNT", "oauth-client"),
			),
		}, os.Getenv("DISCORD_BOARD_CHANNEL_ID"))
		srvAPI.SetPastChannelID(os.Getenv("DISCORD_PAST_CHANNEL_ID"))
		log.Printf("web surface enabled, callback %s", redirect)
	} else {
		log.Print("web surface disabled (DISCORD_OAUTH_REDIRECT_URL unset)")
	}

	// The gateway is what makes Discord's own Interested button feed the
	// roster: GUILD_SCHEDULED_EVENT_USER_ADD is delivered over the socket and
	// nowhere else. Started in the background and retried forever, because
	// everything else here — buttons, rosters, the web page, the interaction
	// endpoint — works without it, and refusing to boot over a socket would
	// take all of that down with it.
	if os.Getenv("DISCORD_GATEWAY_DISABLED") == "" {
		go discordsignup.StartGatewayWithRetry(srvAPI, discordsignup.AuthStoreTokenResolver(
			envOr("AUTH_STORE_URL", "http://127.0.0.1:8303"),
			os.Getenv("AUTH_STORE_TOKEN"),
			envOr("DISCORD_CREDENTIAL_PROVIDER", "discord"),
			envOr("DISCORD_CREDENTIAL_ACCOUNT", "default"),
		))
	} else {
		log.Print("gateway disabled — Discord's Interested button will NOT feed the roster")
	}

	// Events whose time has passed, moved to the archive and their signup cards
	// stripped of buttons. Every five minutes: often enough that a card stops
	// taking signups shortly after the event ends, cheap enough to ignore.
	go func() {
		for range time.Tick(5 * time.Minute) {
			if _, err := srvAPI.CompleteFinishedEvents(); err != nil {
				log.Printf("complete finished events: %v", err)
			}
		}
	}()

	// Expired logins and abandoned login attempts, cleared hourly. Without this
	// oauth_states grows by one row for every login someone starts and never
	// finishes.
	go func() {
		for range time.Tick(time.Hour) {
			if err := store.SweepExpiredSessions(); err != nil {
				log.Printf("sweep sessions: %v", err)
			}
		}
	}()

	mux := http.NewServeMux()
	srvAPI.RegisterHandlers(mux)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("discord-signup-store listening on %s (data=%s)", addr, store.DataDir())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
