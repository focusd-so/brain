package serve

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/validate"
	"github.com/focusd-so/brain/gen/brain/v1/brainv1connect"
	"github.com/focusd-so/brain/gen/common"
	"github.com/focusd-so/brain/internal/auth"
	"github.com/focusd-so/brain/internal/brain"
	"github.com/joho/godotenv"
	"github.com/urfave/cli/v3"

	// 1. Add these imports for H2C support
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

var Command = &cli.Command{
	Name: "serve",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "addr",
			Value:   ":8089",
			Usage:   "address to listen on",
			Aliases: []string{"p"},
		},
		&cli.StringFlag{
			Name:    "turso-db-url",
			Value:   "libsql://focusd-aram.aws-us-east-1.turso.io",
			Sources: cli.EnvVars("FOCUSD_TURSO_CONNECTION_PATH"),
		},
		&cli.StringFlag{
			Name:    "turso-db-token",
			Sources: cli.EnvVars("FOCUSD_TURSO_CONNECTION_TOKEN"),
		},
	},
	Action: func(ctx context.Context, cmd *cli.Command) error {
		err := godotenv.Load()
		if err != nil {
			log.Println("Warning: Error loading .env file")
		}

		url := cmd.String("turso-db-url")
		token := cmd.String("turso-db-token")

		connStr := url
		if token != "" {
			connStr = fmt.Sprintf("%s?authToken=%s", url, token)
		}

		slog.Info("connecting to turso", "url", url)

		sqlDB, err := sql.Open("libsql", connStr)
		if err != nil {
			return fmt.Errorf("failed to open sql connection: %w", err)
		}

		gormDB, err := gorm.Open(sqlite.Dialector{Conn: sqlDB}, &gorm.Config{})
		if err != nil {
			return fmt.Errorf("failed to open gorm connection: %w", err)
		}

		slog.Info("connected to turso", "url", url)

		if err := gormDB.AutoMigrate(&common.UserORM{}, &common.NonceORM{}, &common.PromptHistoryORM{}); err != nil {
			return fmt.Errorf("failed to auto migrate: %w", err)
		}

		// run EngineService as connect rpc handler
		engineService := brain.NewServiceImpl(gormDB)

		mux := http.NewServeMux()
		path, handler := brainv1connect.NewBrainServiceHandler(
			engineService,
			connect.WithInterceptors(
				auth.NewAuthInterceptor(),
				validate.NewInterceptor(),
			),
		)
		mux.Handle(path, handler)

		slog.Info("serving engine service at", "path", path)

		// 2. CRITICAL FIX: Wrap the mux in h2c.NewHandler
		// This forces the server to handle HTTP/2 requests over plaintext
		h2Handler := h2c.NewHandler(mux, &http2.Server{})

		server := &http.Server{
			Addr:    cmd.String("addr"),
			Handler: h2Handler, // Use the wrapped handler here
			// ReadHeaderTimeout is recommended to prevent Slowloris attacks
			ReadHeaderTimeout: 3 * time.Second,
		}

		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt)

		go func() {
			slog.Info("serving engine service", "addr", cmd.String("addr"))
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("failed to serve engine service", "error", err)
				os.Exit(1)
			}
		}()

		<-sigint
		slog.Info("shutting down engine service")

		// Create a timeout context for shutdown
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("server forced to shutdown", "error", err)
		}

		slog.Info("engine service shut down")
		return nil
	},
}
