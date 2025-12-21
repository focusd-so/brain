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

	"connectrpc.com/connect"
	"connectrpc.com/validate"
	"github.com/focusd-so/brain/gen/brain/v1/brainv1connect"
	"github.com/focusd-so/brain/gen/common"
	"github.com/focusd-so/brain/internal/brain"
	"github.com/joho/godotenv"
	"github.com/urfave/cli/v3"
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
			Name:  "turso-db-url",
			Value: "libsql://focusd-aram.aws-us-east-1.turso.io",
		},
		&cli.StringFlag{
			Name:    "turso-db-token",
			Sources: cli.EnvVars("TURSO_DB_TOKEN"),
		},
	},
	Action: func(ctx context.Context, cmd *cli.Command) error {

		err := godotenv.Load()
		if err != nil {
			log.Fatal("Error loading .env file")
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

		if err := gormDB.AutoMigrate(&common.User{}, &common.Nonce{}); err != nil {
			return fmt.Errorf("failed to auto migrate: %w", err)
		}

		// run EngineService as connect rpc handler
		engineService := brain.NewServiceImpl(gormDB)
		p := new(http.Protocols)
		p.SetHTTP1(true)

		mux := http.NewServeMux()
		path, handler := brainv1connect.NewBrainServiceHandler(
			engineService,
			// Validation via Protovalidate is almost always recommended
			connect.WithInterceptors(validate.NewInterceptor()),
		)
		mux.Handle(path, handler)

		slog.Info("serving engine service at", "path", path)

		// Use h2c so we can serve HTTP/2 without TLS.
		p.SetUnencryptedHTTP2(true)
		server := http.Server{
			Addr:      cmd.String("addr"),
			Protocols: p,
			Handler:   mux,
		}

		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt)

		go func() {
			slog.Info("serving engine service")
			if err := server.ListenAndServe(); err != nil {
				slog.Error("failed to serve engine service", "error", err)
			}
			slog.Info("engine service shut down")
		}()

		<-sigint
		slog.Info("shutting down engine service")
		server.Shutdown(context.Background())
		return nil

		return nil
	},
}
