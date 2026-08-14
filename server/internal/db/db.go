// Package db centralises every PostgreSQL operation for the game server.
// It owns the connection pool and exposes clean typed functions — no raw SQL
// outside this package.
package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
)

var conn *sql.DB // unexported — only this package touches the pool directly

// Init opens a PostgreSQL connection and runs all schema migrations.
func Init(dsn string) error {
	var err error
	conn, err = sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	conn.SetMaxOpenConns(25)
	conn.SetMaxIdleConns(5)
	conn.SetConnMaxLifetime(5 * time.Minute)

	if err = conn.Ping(); err != nil {
		return fmt.Errorf("cannot reach PostgreSQL: %w", err)
	}
	return migrate()
}

// DB returns the raw pool for callers that must compose queries (e.g. main.go
// setadmin helper).  Use only when the typed helpers above are insufficient.
func DB() *sql.DB { return conn }

func migrate() error {
	_, err := conn.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id            BIGSERIAL PRIMARY KEY,
			username      TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			email         TEXT DEFAULT '',
			created_at    TIMESTAMPTZ DEFAULT NOW(),
			last_x        REAL DEFAULT 480.0,
			last_y        REAL DEFAULT 320.0,
			gralats       INTEGER DEFAULT 0
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_users_username_lower ON users (LOWER(username));

		CREATE TABLE IF NOT EXISTS sessions (
			token      TEXT PRIMARY KEY,
			user_id    BIGINT NOT NULL,
			username   TEXT NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS chat_history (
			id       BIGSERIAL PRIMARY KEY,
			username TEXT NOT NULL,
			message  TEXT NOT NULL,
			sent_at  TIMESTAMPTZ DEFAULT NOW()
		);
	`)
	if err != nil {
		return err
	}

	migrations := []string{
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS playtime INTEGER DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS body     TEXT DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS head     TEXT DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS hat      TEXT DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS shield   TEXT DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS sword    TEXT DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS is_admin INTEGER DEFAULT 0`,
		// Which map instance the player was on at logout. Empty = default map.
		// Without this, last_x/last_y were restored while the map was reset to
		// the default one, dropping returning players at foreign coordinates.
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS current_map TEXT DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS guild_id BIGINT DEFAULT 0`,
		`CREATE TABLE IF NOT EXISTS inventory (
			user_id  BIGINT NOT NULL,
			item_id  TEXT   NOT NULL,
			quantity INTEGER NOT NULL DEFAULT 1,
			PRIMARY KEY (user_id, item_id)
		)`,
		`CREATE TABLE IF NOT EXISTS world_items (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL,
			sprite_path TEXT NOT NULL DEFAULT '',
			x           REAL NOT NULL DEFAULT 0,
			y           REAL NOT NULL DEFAULT 0,
			price       INTEGER NOT NULL DEFAULT 0,
			item_id     TEXT NOT NULL DEFAULT '',
			map_name    TEXT NOT NULL DEFAULT 'maps/GraalRebornMap.tmx'
		)`,
		`CREATE TABLE IF NOT EXISTS friends (
			user_id    BIGINT NOT NULL,
			friend_id  BIGINT NOT NULL,
			status     TEXT NOT NULL DEFAULT 'pending',
			created_at TIMESTAMPTZ DEFAULT NOW(),
			PRIMARY KEY (user_id, friend_id)
		)`,
		`CREATE TABLE IF NOT EXISTS guilds (
			id          BIGSERIAL PRIMARY KEY,
			name        TEXT UNIQUE NOT NULL,
			tag         TEXT NOT NULL DEFAULT '',
			leader_id   BIGINT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMPTZ DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS guild_members (
			guild_id  BIGINT NOT NULL,
			user_id   BIGINT NOT NULL,
			rank      TEXT NOT NULL DEFAULT 'member',
			joined_at TIMESTAMPTZ DEFAULT NOW(),
			PRIMARY KEY (guild_id, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS player_quests (
			user_id    BIGINT NOT NULL,
			quest_id   TEXT NOT NULL,
			progress   INTEGER NOT NULL DEFAULT 0,
			completed  INTEGER NOT NULL DEFAULT 0,
			started_at TIMESTAMPTZ DEFAULT NOW(),
			PRIMARY KEY (user_id, quest_id)
		)`,
	}
	for _, m := range migrations {
		if _, err := conn.Exec(m); err != nil {
			return fmt.Errorf("migration failed: %w\nSQL: %s", err, m)
		}
	}
	return nil
}

// ── User / session ────────────────────────────────────────────────────────────

// UserRecord carries the fields needed at login / session validation.
type UserRecord struct {
	ID       int64
	Name     string
	LastX    float64
	LastY    float64
	Gralats  int
	Playtime int
	Body     string
	Head     string
	Hat      string
	Shield   string
	Sword    string
	// CurrentMap is the map instance the player was on at logout ("" = default).
	CurrentMap string
}

func CreateUser(username, password, email string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = conn.Exec(
		`INSERT INTO users (username, password_hash, email) VALUES ($1, $2, $3)`,
		username, string(hash), email,
	)
	return err
}

func Authenticate(username, password string) (*UserRecord, error) {
	var u UserRecord
	var hash string
	err := conn.QueryRow(
		`SELECT id, username, password_hash, last_x, last_y, gralats,
		        COALESCE(playtime,0), COALESCE(body,''), COALESCE(head,''),
		        COALESCE(hat,''), COALESCE(shield,''), COALESCE(sword,''),
		        COALESCE(current_map,'')
		 FROM users WHERE LOWER(username) = LOWER($1)`, username,
	).Scan(&u.ID, &u.Name, &hash, &u.LastX, &u.LastY, &u.Gralats,
		&u.Playtime, &u.Body, &u.Head, &u.Hat, &u.Shield, &u.Sword,
		&u.CurrentMap)
	if err != nil {
		return nil, fmt.Errorf("invalid credentials")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return nil, fmt.Errorf("invalid credentials")
	}
	return &u, nil
}

func CreateSession(userID int64, username string) (string, error) {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	_, err := conn.Exec(
		`INSERT INTO sessions (token, user_id, username) VALUES ($1, $2, $3)
		 ON CONFLICT (token) DO UPDATE SET user_id = $2, username = $3`,
		token, userID, username,
	)
	return token, err
}

func ValidateSession(token string) (*UserRecord, error) {
	var u UserRecord
	err := conn.QueryRow(
		`SELECT u.id, u.username, u.last_x, u.last_y, u.gralats,
		        COALESCE(u.playtime,0), COALESCE(u.body,''), COALESCE(u.head,''),
		        COALESCE(u.hat,''), COALESCE(u.shield,''), COALESCE(u.sword,''),
		        COALESCE(u.current_map,'')
		 FROM sessions s JOIN users u ON s.user_id = u.id
		 WHERE s.token = $1`, token,
	).Scan(&u.ID, &u.Name, &u.LastX, &u.LastY, &u.Gralats,
		&u.Playtime, &u.Body, &u.Head, &u.Hat, &u.Shield, &u.Sword,
		&u.CurrentMap)
	if err != nil {
		return nil, fmt.Errorf("invalid session")
	}
	return &u, nil
}

func SaveCosmetics(userID int64, body, head, hat, shield, sword string) {
	conn.Exec(
		`UPDATE users SET body=$1, head=$2, hat=$3, shield=$4, sword=$5 WHERE id=$6`,
		body, head, hat, shield, sword, userID)
}

// UpdatePosition saves the player's logout position together with the map
// instance those coordinates belong to. Saving one without the other puts the
// player back at coordinates that mean nothing on the map they are restored to.
func UpdatePosition(userID int64, x, y float64, currentMap string) {
	conn.Exec(`UPDATE users SET last_x=$1, last_y=$2, current_map=$3 WHERE id=$4`,
		x, y, currentMap, userID)
}

func AddGralats(userID int64, n int) (int, error) {
	var total int
	err := conn.QueryRow(
		`UPDATE users SET gralats = gralats + $1 WHERE id = $2 RETURNING gralats`,
		n, userID,
	).Scan(&total)
	return total, err
}

func AddPlaytime(userID int64, seconds int) {
	conn.Exec(
		`UPDATE users SET playtime = COALESCE(playtime,0) + $1 WHERE id=$2`,
		seconds, userID)
}

func SaveChat(username, message string) {
	conn.Exec(
		`INSERT INTO chat_history (username, message) VALUES ($1, $2)`,
		username, message)
}

func IsAdmin(userID int64) bool {
	var v int
	conn.QueryRow(`SELECT COALESCE(is_admin,0) FROM users WHERE id=$1`, userID).Scan(&v)
	return v != 0
}

func GetPlayerIDByName(name string) (int64, error) {
	var id int64
	err := conn.QueryRow(
		`SELECT id FROM users WHERE LOWER(username)=LOWER($1)`, name).Scan(&id)
	return id, err
}

func DeductGralats(userID int64, amount int) (int, error) {
	var newTotal int
	err := conn.QueryRow(
		`UPDATE users SET gralats = gralats - $1
		 WHERE id = $2 AND gralats >= $1
		 RETURNING gralats`,
		amount, userID,
	).Scan(&newTotal)
	if err != nil {
		return -1, fmt.Errorf("insufficient gralats")
	}
	return newTotal, nil
}
