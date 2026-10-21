// Command webvpn exposes internal web services on the public internet through a
// Cloudflare Tunnel, behind a single login.
//
// The tunnel listener is loopback only. cloudflared, running on the same
// machine, is the only thing that should ever be able to reach it.
//
// A second listener can be opened on the LAN for the service index and the
// admin page -- see lan_index in the config. It has to be bound to a LAN
// address and never to 0.0.0.0, because this machine has a public address and a
// wildcard bind would put both on the internet.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/LCBRST/webvpn/internal/app"
	"github.com/LCBRST/webvpn/internal/config"
	"github.com/LCBRST/webvpn/internal/portal"
	"github.com/LCBRST/webvpn/internal/store"
)

func main() {
	var (
		configPath = flag.String("config", "config.yaml", "path to the YAML config file")
		check      = flag.Bool("check", false, "load and validate the config, then exit")
		hash       = flag.Bool("hash", false, "read a password from stdin and print its bcrypt hash")
		genKey     = flag.Bool("genkey", false, "print a new random session signing key")
		verbose    = flag.Bool("v", false, "log at debug level")
	)
	flag.Parse()

	var err error
	switch {
	case *hash:
		err = runHash()
	case *genKey:
		err = runGenKey()
	default:
		err = runServer(*configPath, *check, *verbose)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "webvpn:", err)
		os.Exit(1)
	}
}

func runServer(path string, checkOnly, verbose bool) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if checkOnly {
		return checkConfig(cfg)
	}

	handler, err := app.New(cfg, logger)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           app.Middleware(logger, handler),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		IdleTimeout:       cfg.Server.IdleTimeout.Std(),
		MaxHeaderBytes:    1 << 20,
		// WriteTimeout is left at zero on purpose. Any non-zero value cuts
		// server-sent events and long-polls off mid-stream, and WebSocket
		// traffic would hide that because hijacked connections ignore it.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	// The LAN index is a second listener rather than another host on the first
	// one. The first is loopback-only and stays that way; this one has to be
	// reachable from the network. Keeping them separate is also what lets the
	// LAN handler stay ignorant of sessions and proxying.
	var lanSrv *http.Server
	if cfg.LANIndexEnabled() {
		lanSrv = &http.Server{
			Addr:              cfg.LANIndex.Listen,
			Handler:           app.LANMiddleware(logger, handler.LANHandler()),
			ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
			IdleTimeout:       cfg.Server.IdleTimeout.Std(),
			MaxHeaderBytes:    1 << 20,
			// WriteTimeout stays zero here too, for the same reason as above.
			ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// One slot per listener, so a failed bind still surfaces when the other one
	// is serving happily.
	serveErr := make(chan error, 2)
	go func() {
		logger.Info("listening",
			"addr", cfg.Server.Listen,
			"portal", cfg.Server.PublicScheme+"://"+cfg.Server.PortalHost,
			"services", handler.ServiceCount())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	if lanSrv != nil {
		go func() {
			logger.Info("listening (lan index)",
				"addr", cfg.LANIndex.Listen,
				"services", handler.ServiceCount())
			if err := lanSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- fmt.Errorf("lan index listener: %w", err)
			}
		}()
	}

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var errs []error
	if err := srv.Shutdown(shutdownCtx); err != nil {
		errs = append(errs, fmt.Errorf("shutdown: %w", err))
	}
	if lanSrv != nil {
		if err := lanSrv.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown lan index: %w", err))
		}
	}
	return errors.Join(errs...)
}

// checkConfig reports whether this config would start, without starting
// anything and, more to the point, without writing anything.
//
// It deliberately does not go through app.New. Building the app seeds the state
// file when there is not one yet, so -check would carry out the migration on a
// machine it was only supposed to be inspecting -- and it is run from push.py
// against a config that is not ours to change.
func checkConfig(cfg *config.Config) error {
	// Templates are embedded and parsed at startup rather than at build time, so
	// the only place a broken one shows up is here.
	if _, err := portal.NewViews(); err != nil {
		return err
	}
	// The rules the store applies to a candidate state: the same user and
	// service checks, plus the ones only it enforces, such as there being at
	// least one admin.
	if err := store.Validate(cfg); err != nil {
		return err
	}

	fmt.Printf("config ok: %d service(s), portal at %s://%s",
		len(cfg.Services), cfg.Server.PublicScheme, cfg.Server.PortalHost)
	if cfg.LANIndexEnabled() {
		fmt.Printf(", lan index at http://%s", cfg.LANIndex.Listen)
	}
	fmt.Printf(", state at %s\n", cfg.State.Path)
	return nil
}

func runHash() error {
	// One reader for both prompts. A fresh bufio.Reader per call would buffer
	// the whole of stdin on the first read and leave the second looking at
	// EOF.
	in := bufio.NewReader(os.Stdin)

	fmt.Fprint(os.Stderr, "Password: ")
	password, err := readLine(in)
	if err != nil {
		return err
	}
	if password == "" {
		return errors.New("empty password")
	}

	fmt.Fprint(os.Stderr, "Repeat:   ")
	again, err := readLine(in)
	if err != nil {
		return err
	}
	if password != again {
		return errors.New("passwords do not match")
	}

	// Read from stdin rather than taken as an argument: anything in argv is
	// visible to every user on the machine through ps.
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return err
	}
	fmt.Println(string(hash))
	return nil
}

func readLine(in *bufio.Reader) (string, error) {
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	// A final line without a trailing newline is still a valid entry.
	return strings.TrimRight(line, "\r\n"), nil
}

func runGenKey() error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(key))
	return nil
}
