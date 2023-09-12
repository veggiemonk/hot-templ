package main

//go:generate templ generate

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/alexedwards/scs/v2"
	conf "github.com/ardanlabs/conf/v3"
	_ "go.uber.org/automaxprocs"
)

//go:embed assets assets/favicon
var assets embed.FS

const serviceName = "hot-templ"

func main() {
	log := makeLogger(os.Stderr)

	if err := run(log); err != nil {
		log.Error("error", "err", err)
		os.Exit(1) //nolint:gocritic
	}
}

type State struct {
	Count int
}

func run(log *slog.Logger) error {
	cfg := struct {
		conf.Version
		Port            int           `conf:"default:8080,env:PORT"`
		Host            string        `conf:"default:127.0.0.1"` // 0.0.0.0 cause OSX to ask "allow connection" as the binary is not signed.
		HealthPath      string        `conf:"default:/healthz"`
		VersionPath     string        `conf:"default:/version"`
		ReadTimeout     time.Duration `conf:"default:5s"`
		WriteTimeout    time.Duration `conf:"default:10s"`
		IdleTimeout     time.Duration `conf:"default:120s"`
		ShutdownTimeout time.Duration `conf:"default:5s"`
	}{
		Version: conf.Version{
			Build: commit,
			Desc:  serviceName + " web service",
		},
	}

	help, err := conf.Parse("", &cfg)
	if err != nil {
		if errors.Is(err, conf.ErrHelpWanted) {
			fmt.Println(help)
			return nil
		}
		return fmt.Errorf("parsing config: %w", err)
	}

	// closing signal
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	cfgStr, err := conf.String(&cfg)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Runtime
	ms := &runtime.MemStats{}
	runtime.ReadMemStats(ms)
	log.Info(
		"starting service...",
		slog.Int64("startup", time.Now().UTC().Unix()),
		slog.Int("cpu", runtime.NumCPU()),
		slog.String("memory", fmt.Sprintf("%d MB", ms.Sys/1024)),
		slog.String("config", cfgStr),
	)
	defer log.Info("service stopped")

	// HTTP
	sm := http.NewServeMux()
	sm.HandleFunc(cfg.VersionPath, VersionInfo)
	sm.HandleFunc(cfg.HealthPath, healthz)

	// Static files
	static, err := fs.Sub(assets, "assets")
	if err != nil {
		return fmt.Errorf("assets: %w", err)
	}
	sm.Handle("/assets/",
		http.StripPrefix("/assets/", http.FileServer(http.FS(static))))

	// Global
	var global State
	const key = "count"
	sessionManager := scs.New()

	// Root handler
	sm.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Info("request", slog.String("method", r.Method), slog.String("path", r.URL.Path))

		switch r.Method {
		case http.MethodGet:
			// w.Header().Set("Content-Type", "text/html")
			userCount := sessionManager.GetInt(r.Context(), key)
			component := page(global.Count, userCount)
			component.Render(r.Context(), w)
			return
		case http.MethodPost:
			// w.Header().Set("Content-Type", "text/html")
			r.ParseForm()
			if r.Form.Has("global") {
				global.Count++
			}
			if r.Form.Has("user") {
				currentCount := sessionManager.GetInt(r.Context(), key)
				sessionManager.Put(r.Context(), key, currentCount+1)
			}
			userCount := sessionManager.GetInt(r.Context(), key)
			component := page(global.Count, userCount)
			component.Render(r.Context(), w)
			return
		default:
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		}
	})

	srv := &http.Server{
		Addr:                         fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:                      sessionManager.LoadAndSave(sm),
		DisableGeneralOptionsHandler: false,
		// Good practice to set timeouts to avoid Slowloris attacks.
		WriteTimeout:      cfg.WriteTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
	go func(srv *http.Server) {
		if zerr := srv.ListenAndServe(); zerr != nil {
			if errors.Is(zerr, http.ErrServerClosed) {
				return
			}
			log.Error("listen: %v", "err", zerr)
			panic(zerr)
		}
	}(srv)

	<-stopChan
	ctxShutDown, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer func() { cancel() }()

	log.Info("shutting down the service...")
	if err = srv.Shutdown(ctxShutDown); err != nil {
		log.Error("server Shutdown Failed", "err", err)
	}

	return nil
}

// makeLogger returns a logger that writes to w [io.Writer]. If w is nil, os.Stderr is used.
func makeLogger(w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	return slog.New(
		slog.NewJSONHandler(w, &slog.HandlerOptions{
			AddSource:   true,
			ReplaceAttr: logReplace,
		},
		).WithAttrs(
			[]slog.Attr{slog.String("app", serviceName)},
		),
	)
}

// logReplace removes the directory from the source's filename.
func logReplace(groups []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey && len(groups) == 0 {
		return slog.Attr{
			Key:   slog.TimeKey,
			Value: slog.Int64Value(time.Now().UTC().Unix()),
		}
	}
	if a.Key == slog.SourceKey && len(groups) == 0 {
		return slog.Attr{}
	}
	return a
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, "ok")
}
