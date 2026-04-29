package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	pkgerr "github.com/pkg/errors"
	"gopkg.in/natefinch/lumberjack.v2"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()

	os.Exit(status)
}

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type multiError interface {
	error
	Unwrap() []error
}

func errorAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{
		{Key: "message", Value: slog.StringValue(err.Error())},
	}
	attrs = append(attrs, linkoerr.Attrs(err)...)
	if stackErr, ok := errors.AsType[stackTracer](err); ok {
		attrs = append(attrs, slog.Attr{
			Key:   "stack_trace",
			Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
		})
	}
	return attrs
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}

		if me, ok := errors.AsType[multiError](err); ok {
			var meAttrs []slog.Attr
			for i, e := range me.Unwrap() {
				meAttrs = append(meAttrs, slog.GroupAttrs(fmt.Sprintf("error_%d", i+1), errorAttrs(e)...))
			}
			return slog.GroupAttrs("errors", meAttrs...)
		}

		return slog.GroupAttrs("error", errorAttrs(err)...)
	}
	return a
}

type closeFunc func() error

func initiliazeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	handlers := []slog.Handler{
		tint.NewHandler(os.Stderr, &tint.Options{
			Level:       slog.LevelDebug,
			ReplaceAttr: replaceAttr,
			NoColor:     !isatty.IsTerminal(os.Stderr.Fd()) && !isatty.IsCygwinTerminal(os.Stderr.Fd()),
		}),
	}
	closers := []closeFunc{}

	if logFile != "" {
		rotatingFile := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    1,
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}
		handlers = append(handlers, slog.NewJSONHandler(rotatingFile, &slog.HandlerOptions{
			ReplaceAttr: replaceAttr,
		}))
		closers = append(closers, func() error {
			if err := rotatingFile.Close(); err != nil {
				return fmt.Errorf("failed to close log file: %w", err)
			}
			return nil
		})
	}

	closer := func() error {
		var errs []error
		for _, close := range closers {
			if err := close(); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	return slog.New(slog.NewMultiHandler(handlers...)), closer, nil
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	logger, cf, err := initiliazeLogger(os.Getenv("LINKO_LOG_FILE"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not initialise logger: %v", err)
		return 1
	}
	defer func() {
		if err := cf(); err != nil {
			fmt.Fprintf(os.Stderr, "Could not clean up logger on close: %v", err)
		}
	}()

	env := os.Getenv("ENV")
	hostname, _ := os.Hostname()
	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to create store: %v", err))
		return 1
	}
	logger.Debug(fmt.Sprintf("Linko is running on http://localhost:%d", httpPort))
	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error(fmt.Sprintf("failed to shutdown server: %v", err))
		return 1
	}
	if serverErr != nil {
		logger.Info(fmt.Sprintf("server error: %v", serverErr))
		return 1
	}

	logger.Debug(fmt.Sprintf("Linko is shutting down"))
	return 0
}
