package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"esim/internal/at"
	"esim/internal/buildinfo"
	"esim/internal/config"
	"esim/internal/httpapi"
	"esim/internal/serial"
	"esim/internal/service"
	"esim/internal/store"
)

func main() {
	syscall.Umask(0077)
	if e := run(); e != nil {
		slog.Error("daemon stopped", "reason", e.Error())
		os.Exit(1)
	}
}
func run() error {
	version := flag.Bool("version", false, "print version and build information without loading configuration")
	path := flag.String("config", "configs/simd.example.yaml", "YAML configuration")
	check := flag.Bool("check-config", false, "validate configuration and environment without opening database or serial ports")
	probe := flag.String("probe-port", "", "probe a serial port with identification queries only; does not initialize SMS or read/delete/send SMS")
	probeSMS := flag.Bool("probe-sms", false, "include read-only SIM, registration and SMS capability queries; SIM identity is masked")
	baud := flag.Int("probe-baud", 115200, "serial probe baud rate")
	flag.Parse()
	if *version {
		fmt.Printf("simd %s (commit %s, built %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
		return nil
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *probe != "" {
		return probePort(ctx, *probe, *baud, *probeSMS)
	}
	c, e := config.Load(*path)
	if e != nil {
		return e
	}
	if *check {
		fmt.Println("configuration valid")
		return nil
	}
	s, e := store.Open(c.Database)
	if e != nil {
		return e
	}
	defer s.Close()
	a := service.New(c, s)
	a.Start(ctx)
	server := &http.Server{Addr: c.Listen, Handler: httpapi.New(a), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	slog.Info("daemon started", "listen", c.Listen, "devices", len(c.Devices))
	select {
	case <-ctx.Done():
	case e = <-a.Fatal():
		cancel()
	case e = <-result:
		if !errors.Is(e, http.ErrServerClosed) {
			slog.Error("HTTP listener stopped")
		}
		cancel()
	}
	cancel()
	shutdown, done := context.WithTimeout(context.Background(), 15*time.Second)
	defer done()
	server.Shutdown(shutdown)
	a.Wait()
	if e != nil && !errors.Is(e, http.ErrServerClosed) {
		return errors.New("service worker or HTTP listener failed")
	}
	return nil
}
func probePort(ctx context.Context, path string, baud int, smsCapabilities bool) error {
	p, e := serial.Open(path, baud, false)
	if e != nil {
		return fmt.Errorf("serial open failed: %w", e)
	}
	s := at.New(p)
	defer s.Close()
	results := map[string]any{}
	commands := []string{"AT", "AT+CGMM", "AT+CGMR"}
	if smsCapabilities {
		commands = append(commands, "AT+CPIN?", "AT+MCCID", "AT+CEREG?", "AT+CMGF=?", "AT+CMGF?", "AT+CPMS=?", "AT+CPMS?", "AT+CNMI=?")
	}
	for _, cmd := range commands {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		r, e := s.Do(c, cmd)
		cancel()
		if e != nil {
			return fmt.Errorf("identification probe failed at %s", cmd)
		}
		if cmd == "AT+MCCID" {
			results[cmd] = "SIM identity available (redacted)"
		} else {
			results[cmd] = r
		}
	}
	return json.NewEncoder(os.Stdout).Encode(results)
}
