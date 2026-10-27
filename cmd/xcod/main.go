// xcod: live MPEG-TS restreamer. One provider connection per channel, shared by every viewer
// (plain TS or HLS), with an admin panel at /admin.
//
// Environment variables:
//
//	XCOD_DATA         data directory (config.json, stats.json, admin.json); default ./data (/data in Docker)
//	XCOD_LISTEN       HTTP address; default :8090
//	ADMIN_USER        initial panel user (default admin)
//	ADMIN_PASSWORD    initial panel password; without it, one is generated and shown in the log
//	XCOD_RESET_ADMIN  "1" recreates the panel login from ADMIN_USER/ADMIN_PASSWORD
//	XCOD_HTTP_LISTEN  port 80 (e.g. ":80"): certificate challenge and access through the domain
//	XCOD_HTTPS_LISTEN port 443 (e.g. ":443"): automatic HTTPS for the domain set in the panel
//	XCOD_UPDATE_DIR   enables updates from the panel (set by the installer)
//	XCOD_UPDATE_REPO  GitHub repository checked for releases (default hubxcod/xcod-serve)
//
// Applied on startup (the installer only uses them on the first run):
//
//	XCOD_ORIGIN, XCOD_ORIGIN_TOKEN  become a repeater of this primary
//	XCOD_DOMAIN                     domain with automatic HTTPS
//	XCOD_PANEL_TOKEN                secret path of the panel (/<token>/admin)
//	XCOD_CAPACITY_MBPS              outbound bandwidth used to split the audience
package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xcod/internal/admin"
	"xcod/internal/auth"
	"xcod/internal/certs"
	"xcod/internal/cluster"
	"xcod/internal/config"
	"xcod/internal/stats"
	"xcod/internal/stream"
	"xcod/internal/update"
)

// version is set at build time: -ldflags "-X main.version=v1.2.0"
var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadDotEnv reads KEY=VALUE from a .env in the current directory (local use). Variables
// already set in the environment take precedence.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\xef\xbb\xbf"))
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}

// bootstrap applies the first-run variables (installer) to the configuration.
func bootstrap(store *config.Store) error {
	cfg := store.Get()
	if o, t := os.Getenv("XCOD_ORIGIN"), os.Getenv("XCOD_ORIGIN_TOKEN"); o != "" && (o != cfg.Origin || t != cfg.OriginToken || !cfg.IsRepeater()) {
		if err := cluster.Connect(store, strings.TrimRight(o, "/"), t); err != nil {
			return fmt.Errorf("XCOD_ORIGIN: %v", err)
		}
		log.Printf("cluster: this server is now a repeater of %s", o)
	}
	return store.Update(func(c *config.Config) error {
		if d := os.Getenv("XCOD_DOMAIN"); d != "" {
			c.Domain = d
		}
		if t := os.Getenv("XCOD_PANEL_TOKEN"); t != "" {
			c.PanelToken = t
		}
		if v := os.Getenv("XCOD_CAPACITY_MBPS"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("invalid XCOD_CAPACITY_MBPS: %q", v)
			}
			c.CapacityMbps = n
		}
		return nil
	})
}

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	loadDotEnv(".env")
	dataDir := env("XCOD_DATA", "data") // in Docker: /data (set in the Dockerfile)
	listen := env("XCOD_LISTEN", ":8090")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatalf("data directory %s: %v", dataDir, err)
	}

	store, err := config.Load(filepath.Join(dataDir, "config.json"))
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}
	creds, generated, err := auth.LoadCredentials(filepath.Join(dataDir, "admin.json"),
		os.Getenv("ADMIN_USER"), os.Getenv("ADMIN_PASSWORD"), os.Getenv("XCOD_RESET_ADMIN") == "1")
	if err != nil {
		log.Fatalf("panel login: %v", err)
	}
	if generated != "" {
		log.Printf("==> panel: user %q, generated password %q (change it under Security; it will not be shown again)",
			creds.Username(), generated)
	}

	if err := bootstrap(store); err != nil {
		log.Fatalf("initial configuration: %v", err)
	}

	httpsListen := os.Getenv("XCOD_HTTPS_LISTEN")
	meter := stats.New(filepath.Join(dataDir, "stats.json"))
	hub := stream.NewHub(store, meter)
	tlsm := certs.New(store, filepath.Join(dataDir, "certs"), httpsListen)
	updater := update.New(os.Getenv("XCOD_UPDATE_DIR"), version)
	if msg, at := updater.Result(); msg != "" && time.Since(at) < time.Minute {
		log.Printf("update: %s", msg)
	}
	releases := update.NewReleases(os.Getenv("XCOD_UPDATE_REPO"))
	registry := cluster.NewRegistry(store, hub, version)
	hub.SetBalancer(registry)
	_, port, _ := net.SplitHostPort(listen)
	agent := cluster.NewAgent(store, hub, port, version, func() string {
		return cmp.Or(store.Get().PublicURL, tlsm.URL())
	}, updater)
	panel, err := admin.New(store, hub, meter, creds, admin.Network{Registry: registry, Agent: agent, Certs: tlsm, Updater: updater, Releases: releases, Version: version})
	if err != nil {
		log.Fatalf("panel: %v", err)
	}

	mux := http.NewServeMux()
	hub.Routes(mux)
	registry.Routes(mux)
	panel.Routes(mux)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if store.Get().PanelToken != "" { // never point anyone to the hidden panel
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/admin", http.StatusFound)
	})
	handler := panel.Gate(mux)

	// statistics count the audience of the whole network (this server + repeaters)
	load := func() map[string]stats.ChannelLoad {
		l := hub.Load()
		for id, n := range registry.Viewers() {
			x := l[id]
			x.Viewers += n
			l[id] = x
		}
		return l
	}
	stop := make(chan struct{})
	go meter.Run(load, stop)
	go registry.Run(stop)
	go agent.Run(stop)
	go tlsm.Watch(stop)
	if !store.Get().IsRepeater() { // repeaters update from the primary
		go releases.Watch(stop, version)
	}

	newServer := func(addr string, h http.Handler) *http.Server {
		return &http.Server{
			Addr: addr, Handler: h,
			ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10,
			// no global WriteTimeout: TS is an endless stream (each write has its own deadline)
		}
	}
	srv := newServer(listen, handler)
	var extra []*http.Server
	if addr := os.Getenv("XCOD_HTTP_LISTEN"); addr != "" {
		extra = append(extra, newServer(addr, tlsm.HTTPHandler(handler)))
	}
	if httpsListen != "" {
		s := newServer(httpsListen, handler)
		s.TLSConfig = tlsm.TLSConfig()
		extra = append(extra, s)
	}
	for _, s := range extra {
		go func(s *http.Server) {
			var err error
			if s.TLSConfig != nil {
				var ln net.Listener
				if ln, err = net.Listen("tcp", s.Addr); err == nil {
					err = s.Serve(tls.NewListener(ln, s.TLSConfig))
				}
			} else {
				err = s.ListenAndServe()
			}
			if !errors.Is(err, http.ErrServerClosed) {
				log.Printf("port %s: %v", s.Addr, err)
			}
		}(s)
	}

	// SIGTERM or Ctrl+C: save the statistics before exiting
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		log.Printf("shutting down: saving statistics")
		close(stop)
		meter.Record(hub.Load, time.Now())
		srv.Close() // TS streams never end: no point waiting for them
		for _, s := range extra {
			s.Close()
		}
	}()

	cfg := store.Get()
	mode := "primary"
	if cfg.IsRepeater() {
		mode = "repeater of " + cfg.Origin
	}
	log.Printf("xcod %s listening on %s (%s) with %d channel(s); data in %s; panel at /admin",
		version, listen, mode, len(cfg.Channels), dataDir)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
