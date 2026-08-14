package main

import (
	"darkzone/MultiTestServer/internal/db"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	setAdmin := flag.String("setadmin", "", "Set a user as admin by username and exit")
	flag.Parse()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/graalreborn?sslmode=disable"
	}

	if err := db.Init(dsn); err != nil {
		log.Fatalf("[DB] Initialization error: %v", err)
	}
	log.Println("[DB] PostgreSQL connected")

	// -setadmin: grant admin rights to a user and exit immediately.
	if *setAdmin != "" {
		res, err := db.DB().Exec(`UPDATE users SET is_admin=1 WHERE LOWER(username)=LOWER($1)`, *setAdmin)
		if err != nil {
			log.Fatalf("[setadmin] DB error: %v", err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			log.Fatalf("[setadmin] User %q not found in database", *setAdmin)
		}
		log.Printf("[setadmin] User %q is now an admin.", *setAdmin)
		return
	}

	// Must run before newHub(): built-in NPCs and gralats are placed on defaultMap.
	defaultMap = resolveDefaultMap("config.json")
	log.Printf("[HUB] Default map instance: %s", defaultMap)

	globalHub = newHub()
	newLuaManager(globalHub)
	go globalHub.runGameLoop()
	log.Println("[HUB] Game loop started (60 Hz)")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/register", handleRegister)
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/assets/list", handleAssetsList)
	mux.HandleFunc("/api/maps/chunk", handleMapChunk)
	mux.HandleFunc("/api/maps/gmap", handleMapGMap)
	mux.HandleFunc("/ws", handleWebSocket)
	// Game asset directories (served relative to the project root where the
	// server binary is expected to run).
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir("assets"))))
	mux.Handle("/maps/", http.StripPrefix("/maps/", http.FileServer(http.Dir("maps"))))
	mux.Handle("/GANITEMPLATE/", http.StripPrefix("/GANITEMPLATE/", http.FileServer(http.Dir("GANITEMPLATE"))))

	staticDir := "server/static"
	staticFS := http.FileServer(http.Dir(staticDir))

	// Catch-all: fall back to server/static (index.html, game.wasm, wasm_exec.js).
	// Game assets are served via dedicated routes: /assets/, /maps/, /GANITEMPLATE/.
	if _, err := os.Stat(staticDir); err == nil {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			staticFS.ServeHTTP(w, r)
		})
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, "Go Multiplayer Server — run the native client or compile to WASM.")
		})
	}

	addr := ":" + port
	log.Printf("╔══════════════════════════════════════╗")
	log.Printf("║     GO MULTIPLAYER SERVER            ║")
	log.Printf("╠══════════════════════════════════════╣")
	log.Printf("║  HTTP  : http://localhost%s         ║", addr)
	log.Printf("║  WS    : ws://localhost%s/ws        ║", addr)
	log.Printf("║  API   : /api/register  /api/login  ║")
	log.Printf("╚══════════════════════════════════════╝")

	if err := http.ListenAndServe(addr, corsMiddleware(mux)); err != nil {
		log.Fatal(err)
	}
}
