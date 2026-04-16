package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/store"
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

type closeFunc func() error

func initiliazeLogger(logFile string) (*log.Logger, closeFunc, error) {
	lFile := os.Getenv(logFile)
	if lFile == "" {
		return log.New(os.Stderr, "", log.LstdFlags), func() error { return nil }, nil
	}

	fh, err := os.OpenFile(lFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, func() error { return nil }, err
	}
	bfh := bufio.NewWriterSize(fh, 8192)

	mw := io.MultiWriter(os.Stderr, bfh)

	var cf = func() error {
		return bfh.Flush()
	}

	return log.New(mw, "", log.LstdFlags), cf, nil
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
		logger.Printf("failed to create store: %v", err)
		return 1
	}
	logger.Printf("Linko is running on http://localhost:%d", httpPort)
	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Printf("failed to shutdown server: %v", err)
		return 1
	}
	if serverErr != nil {
		logger.Printf("server error: %v", serverErr)
		return 1
	}

	logger.Printf("Linko is shutting down")
	return 0
}
