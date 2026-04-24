package main

import (
	"bufio"
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
	pkgerr "github.com/pkg/errors"
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
	debugHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
	})

	lFile := os.Getenv(logFile)
	if lFile == "" {
		lh := slog.NewTextHandler(os.Stderr, nil)
		return slog.New(lh), func() error { return nil }, nil
	}

	fh, err := os.OpenFile(lFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, func() error { return nil }, err
	}
	bfh := bufio.NewWriterSize(fh, 8192)
	infoHandler := slog.NewJSONHandler(bfh, &slog.HandlerOptions{
		Level:       slog.LevelInfo,
		ReplaceAttr: replaceAttr,
	})

	var cf = func() error {
		return bfh.Flush()
	}

	logger := slog.New(slog.NewMultiHandler(
		debugHandler,
		infoHandler,
	))
	env := os.Getenv("ENV")
	hostname, _ := os.Hostname()
	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)

	return logger, cf, nil
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	logger, cf, err := initiliazeLogger("LINKO_LOG_FILE")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not initialise logger: %v", err)
		return 1
	}
	defer func() {
		err := cf()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Could not clean up logger on close: %v", err)
		}
	}()

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
