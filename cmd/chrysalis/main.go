// Command chrysalis is the Chrysalis sandboxed Python execution engine.
//
// Usage:
//
//	chrysalis serve --wasm wasm/python.wasm --filter-dir filters --port 8080
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/bkmashiro/chrysalis/internal/handler"
	"github.com/bkmashiro/chrysalis/internal/pool"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: chrysalis <command>\ncommands: serve\n")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	wasmPath    := fs.String("wasm", "wasm/python.wasm", "path to python.wasm")
	filterDir   := fs.String("filter-dir", "filters", "directory containing YAML filter profiles")
	port        := fs.Int("port", 8080, "HTTP listen port")
	sockPath    := fs.String("sock", "/tmp/chrysalis_worker.sock", "base Unix socket path; pool members append .1, .2, ...")
	workerScript := fs.String("worker", "shim/worker.py", "path to worker.py")
	shimScript  := fs.String("shim", "shim/bootstrap.py", "path to bootstrap.py")
	workers     := fs.Int("workers", 4, "size of the Python worker pool (concurrent /run capacity)")
	_ = fs.Parse(args)

	// Read WASM binary.
	wasmBytes, err := os.ReadFile(*wasmPath)
	if err != nil {
		log.Fatalf("chrysalis: read WASM: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Build the pool (compiles WASM, starts worker pool).
	p, err := pool.New(ctx, wasmBytes, *workerScript, *shimScript, *filterDir, *sockPath, *workers)
	if err != nil {
		log.Fatalf("chrysalis: init pool: %v", err)
	}
	defer p.Close(context.Background())

	h := handler.New(p)
	mux := http.NewServeMux()
	mux.Handle("/run", h)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: mux,
	}

	log.Printf("chrysalis: listening on :%d", *port)

	go func() {
		<-ctx.Done()
		log.Println("chrysalis: shutting down")
		_ = srv.Shutdown(context.Background())
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("chrysalis: server: %v", err)
	}
}
