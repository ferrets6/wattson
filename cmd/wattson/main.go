package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata" // embeds the timezone database in the binary: the final Docker image is minimal (distroless), with no /usr/share/zoneinfo

	"github.com/joho/godotenv"

	"github.com/ferrets6/wattson/internal/api"
	"github.com/ferrets6/wattson/internal/attribution"
	"github.com/ferrets6/wattson/internal/auth"
	"github.com/ferrets6/wattson/internal/beszel"
	"github.com/ferrets6/wattson/internal/homeassistant"
	"github.com/ferrets6/wattson/internal/hostcpu"
	"github.com/ferrets6/wattson/internal/mqtt"
	"github.com/ferrets6/wattson/internal/rollup"
	"github.com/ferrets6/wattson/internal/store"
)

func main() {
	_ = godotenv.Load() // in production the env vars are already set by the container; .env is for local development only

	dbPath := getenv("DB_PATH", "./wattson.db")

	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("opening storage: %v", err)
	}
	defer db.Close()
	log.Println("wattson starting, db:", dbPath)

	collector := mqtt.New(db, mqtt.Config{
		BrokerURL: getenv("MQTT_BROKER_URL", ""),
		Username:  getenv("MQTT_USERNAME", ""),
		Password:  getenv("MQTT_PASSWORD", ""),
		Topic:     getenv("MQTT_TOPIC", ""),
	})
	if err := collector.Start(); err != nil {
		log.Fatalf("starting mqtt collector: %v", err)
	}
	defer collector.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	beszelClient := beszel.New(db, beszel.Config{
		URL:           getenv("BESZEL_URL", ""),
		AdminEmail:    getenv("BESZEL_ADMIN_EMAIL", ""),
		AdminPassword: getenv("BESZEL_ADMIN_PASSWORD", ""),
	})
	go beszelClient.Start(ctx)

	go hostcpu.Start(ctx, db, 2*time.Second)

	haClient := homeassistant.New(db, homeassistant.Config{
		URL:         getenv("HA_URL", ""),
		Token:       getenv("HA_TOKEN", ""),
		PunEntityID: getenv("HA_PUN_ENTITY_ID", ""),
	})
	go haClient.Start(ctx)

	attrCfg, err := attribution.LoadConfig(getenv("ATTRIBUTION_CONFIG_PATH", ""))
	if err != nil {
		log.Fatalf("attribution config: %v", err)
	}
	go rollup.Start(ctx, db, rollup.Config{Attribution: attrCfg})

	defaultSpread, err := strconv.ParseFloat(getenv("DEFAULT_SPREAD_EUR_KWH", "0.10"), 64)
	if err != nil {
		log.Fatalf("invalid DEFAULT_SPREAD_EUR_KWH: %v", err)
	}

	authenticator, err := auth.New(ctx, auth.Config{
		IssuerURL:    getenv("OIDC_ISSUER_URL", ""),
		ClientID:     getenv("OIDC_CLIENT_ID", ""),
		ClientSecret: getenv("OIDC_CLIENT_SECRET", ""),
		RedirectURL:  getenv("OIDC_REDIRECT_URL", ""),
		SecretKey:    getenv("SESSION_SECRET_KEY", ""),
	})
	if err != nil {
		log.Fatalf("auth configuration: %v", err)
	}
	if !authenticator.Enabled() {
		log.Println("auth: OIDC_ISSUER_URL not set, no application-level authentication (assumes a forward-auth in front, e.g. Authelia)")
	}

	mux := http.NewServeMux()
	api.RegisterRoutes(mux, db, defaultSpread)
	mux.HandleFunc("GET /login", authenticator.LoginHandler)
	mux.HandleFunc("GET /callback", authenticator.CallbackHandler)
	mux.HandleFunc("GET /logout", authenticator.LogoutHandler)
	mux.Handle("/", http.FileServer(http.FS(webRoot())))

	httpServer := &http.Server{
		Addr:    getenv("LISTEN_ADDR", ":8080"),
		Handler: authenticator.Protect(mux),
	}
	go func() {
		log.Println("http: listening on", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("wattson: shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Println("http: unclean shutdown:", err)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
