package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/gorilla/mux"
	"github.com/patrickmn/go-cache"
	"github.com/rs/zerolog"
	_ "modernc.org/sqlite"
)

type server struct {
	db     *sql.DB
	router *mux.Router
	exPath string
}

var (
	address     = flag.String("address", "0.0.0.0", "Bind IP Address")
	port        = flag.String("port", "8080", "Listen Port")
	waDebug     = flag.String("wadebug", "", "Enable whatsmeow debug (INFO or DEBUG)")
	logType     = flag.String("logtype", "console", "Type of log output (console or json)")
	colorOutput = flag.Bool("color", false, "Enable colored output for console logs")
	sslcert     = flag.String("sslcertificate", "", "SSL Certificate File")
	sslprivkey  = flag.String("sslprivatekey", "", "SSL Certificate Private Key File")
	adminToken  = flag.String("admintoken", "", "Security Token to authorize admin actions")
	container   *sqlstore.Container

	killchannel   = make(map[int](chan bool))
	userinfocache = cache.New(5*time.Minute, 10*time.Minute)
	log           zerolog.Logger
)

func init() {
	flag.Parse()

	if *logType == "json" {
		log = zerolog.New(os.Stdout).With().Timestamp().Str("role", filepath.Base(os.Args[0])).Logger()
	} else {
		output := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339, NoColor: !*colorOutput}
		log = zerolog.New(output).With().Timestamp().Str("role", filepath.Base(os.Args[0])).Logger()
	}

	if *adminToken == "" {
		if v := os.Getenv("WUZAPI_ADMIN_TOKEN"); v != "" {
			*adminToken = v
		}
	}
}

func getWritableDbPath() string {
	dbPath := "dbdata"

	if err := os.MkdirAll(dbPath, 0755); err != nil {
		log.Fatal().Err(err).Msg("Failed to create dbdata directory")
		os.Exit(1)
	}
	return dbPath
}

	// Fallback to /tmp
	tmpFallback := filepath.Join(os.TempDir(), "wuzapi-dbdata")
	log.Warn().Msg("Using fallback path: " + tmpFallback)
	if err := os.MkdirAll(tmpFallback, 0755); err != nil {
		log.Fatal().Err(err).Msg("Could not create fallback dbdata directory")
		os.Exit(1)
	}
	return tmpFallback
}

func main() {
	dbDir := getWritableDbPath()

	usersDbPath := filepath.Join(dbDir, "users.db")
	mainDbPath := "file:" + filepath.Join(dbDir, "main.db") + "?_pragma=foreign_keys(1)&_busy_timeout=3000"

	db, err := sql.Open("sqlite", usersDbPath+"?_pragma=foreign_keys(1)&_busy_timeout=3000")
	if err != nil {
		log.Fatal().Err(err).Msg("Could not open/create users.db")
		os.Exit(1)
	}
	defer db.Close()

	sqlStmt := `CREATE TABLE IF NOT EXISTS users (
		id INTEGER NOT NULL PRIMARY KEY,
		name TEXT NOT NULL,
		token TEXT NOT NULL,
		webhook TEXT NOT NULL default "",
		jid TEXT NOT NULL default "",
		qrcode TEXT NOT NULL default "",
		connected INTEGER,
		expiration INTEGER,
		events TEXT NOT NULL default "All"
	);`
	if _, err := db.Exec(sqlStmt); err != nil {
		panic(fmt.Sprintf("%q: %s\n", err, sqlStmt))
	}

	if *waDebug != "" {
		dbLog := waLog.Stdout("Database", *waDebug, *colorOutput)
		container, err = sqlstore.New("sqlite", mainDbPath, dbLog)
	} else {
		container, err = sqlstore.New("sqlite", mainDbPath, nil)
	}
	if err != nil {
		panic(err)
	}

	s := &server{
		router: mux.NewRouter(),
		db:     db,
		exPath: dbDir,
	}
	s.routes()
	s.connectOnStartup()

	srv := &http.Server{
		Addr:              *address + ":" + *port,
		Handler:           s.router,
		ReadHeaderTimeout: 20 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       180 * time.Second,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if *sslcert != "" {
			if err := srv.ListenAndServeTLS(*sslcert, *sslprivkey); err != nil && err != http.ErrServerClosed {
				log.Fatal().Err(err).Msg("Startup failed (TLS)")
			}
		} else {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatal().Err(err).Msg("Startup failed")
			}
		}
	}()

	log.Info().Str("address", *address).Str("port", *port).Msg("Server Started")
	<-done
	log.Info().Msg("Server Stopped")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("Server Shutdown Failed")
		os.Exit(1)
	}

	log.Info().Msg("Server Exited Properly")
}
