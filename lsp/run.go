package lsp

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"adventurers-life-online/server/internal/engine/language"
	"adventurers-life-online/server/internal/game/scripting"
	"adventurers-life-online/server/pkg/auth"
	"adventurers-life-online/server/pkg/config"
	pb "adventurers-life-online/server/proto/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Run serves the language server on /lsp until ctx is cancelled. Compose starts it in build mode only.
func Run(ctx context.Context, cfg *config.LSPConfig) error {
	library := scripting.FollowAssets(ctx, cfg.AssetsURL)
	if library == nil {
		library = scripting.NewLibrary(scripting.Static{}, nil) // no asset service: only open documents
	}
	revocations := auth.NewRevocations()
	if cfg.WorldGRPCAddr != "" {
		conn, err := grpc.NewClient(cfg.WorldGRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return err
		}
		defer conn.Close()
		go auth.WatchRevocations(ctx, pb.NewWorldServiceClient(conn), revocations, 5*time.Second)
	}
	srv := NewServer(library, auth.NewTokenManager(cfg.JWTSecret, 24*time.Hour), revocations.Revoked,
		[]*language.HostModule{scripting.GameModule()})

	mux := http.NewServeMux()
	mux.Handle("/lsp", srv)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "healthy", "service": "lsp", "scripts": len(library.List())})
	})
	server := &http.Server{Addr: ":" + cfg.Port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("[LSP] Listening on ws://localhost:%s/lsp (scripts from %s)", cfg.Port, cfg.AssetsURL)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("lsp server failed: %v", err)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}
