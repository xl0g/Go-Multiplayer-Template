package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"
)

const (
	screenW = 800
	screenH = 600

	tileSize  = 16 // GraalRebornMap.tmx tile size in pixels
	mapTilesW = 70 // map width in tiles
	mapTilesH = 70 // map height in tiles

	worldW = mapTilesW * tileSize // 1120
	worldH = mapTilesH * tileSize // 1120

	// zoomMin and zoomMax are now in Cfg (config.json).
	// world buffer must be large enough to hold a full viewport at max zoom-out
	worldBufW = 2320 // int(screenW/zoomMin)+32 = int(800/0.35)+32
	worldBufH = 1748 // int(screenH/zoomMin)+32 = int(600/0.35)+32
)

// Game implements ebiten.Game and acts as the top-level state machine.
type Game struct {
	state GameState

	// Assets
	bodyImg *ebiten.Image
	headImg *ebiten.Image

	// Menus
	mainMenu      *MainMenu
	loginMenu     *LoginMenu
	regMenu       *RegisterMenu
	cosmeticMenu  *CosmeticMenu
	inventoryMenu *InventoryMenu
	adminMenu     *AdminMenu

	// Network (nil while disconnected)
	conn *Connection

	// Local player info
	localID        string
	localName      string
	localGralats   int
	localInventory []InventoryItem
	isAdmin        bool

	// World state
	localChar    *Character
	otherPlayers map[string]*Character
	npcs map[string]*Character
	mu   sync.Mutex

	// Chat
	chat *Chat

	// TMX map (single-file mode)
	gameMap           *GameMap
	currentMapName    string
	prevMapName       string
	mapSwitchCooldown float64

	// GMAP chunk-based world (used when a .gmap is active)
	chunkMgr     *ChunkManager
	activeGMap   string        // "" when not in chunk mode
	prevGMap     string        // saved GMAP name when entering a building
	prevChunkMgr *ChunkManager // saved ChunkManager (restored on exit)
	prevGMapX    float64       // saved player X in the GMAP
	prevGMapY    float64       // saved player Y in the GMAP

	// World gralat pickups (replicated from server)
	worldGralats []GralatPickup
	grMu         sync.Mutex

	// World items (admin-spawned, replicated from server)
	worldItems      []WorldSpawnItem
	nearWorldItem   *WorldSpawnItem
	worldItemDialog *WorldSpawnItem // item currently shown in the click popup

	// Sprite cache for world-item sprites
	wiSpriteCache  map[string]*ebiten.Image
	tapToBuyImg    *ebiten.Image

	// UI state
	signDialog   string
	npcDialog    string
	npcGralatN   int
	nearNPCID    string
	nearNPCType  int
	profileOpen  bool
	viewedPlayer *Character

	// Profile
	localPlaytime int
	sessionStart  time.Time
	previewImg    *ebiten.Image

	// Push animation delay
	pushTimer float64

	// Noclip mode (disables tile collision for local player)
	noclip bool

	// Main panel menu (slides from top)
	panelMenu *PanelMenu

	// Cached last-sent state to reduce message rate
	lastSentX, lastSentY float64
	lastSentDir          int
	lastSentMoving       bool
	lastSentAnim         string
	lastSentMounted      bool

	// Sword hit tracking (reset each new swing)
	swordHitSent bool

	// Grab state
	grabBtnHeld bool // true while the virtual grab button is held by mouse

	// Player HP (PvP)
	localHP    int
	localMaxHP int
	hpCircles  [3]*ebiten.Image

	// Virtual action button images
	grabKeyImg  *ebiten.Image
	swordKeyImg *ebiten.Image

	lastUpdate time.Time

	// Camera zoom (1.0 = default, <1 = zoomed out, >1 = zoomed in)
	zoom     float64
	worldBuf *ebiten.Image

	// Debug overlay (F3)
	debugOverlay bool

	// Social
	friends        []FriendEntry
	friendRequests []FriendEntry
	myGuild        *GuildInfo
	guildList      []GuildListEntry
	quests         []QuestEntry
	socialPanel    *SocialPanel // unified friends/guilds/quests panel
}

// NewGame initialises the Game. Assets may be nil (graceful fallback).
func NewGame(bodyImg, headImg, tilesImg *ebiten.Image) *Game {
	g := &Game{
		state:         StateMainMenu,
		bodyImg:       bodyImg,
		headImg:       headImg,
		mainMenu:      NewMainMenu(),
		loginMenu:     NewLoginMenu(),
		regMenu:       NewRegisterMenu(),
		cosmeticMenu:  NewCosmeticMenu(),
		inventoryMenu: NewInventoryMenu(),
		adminMenu:     NewAdminMenu(),
		otherPlayers: make(map[string]*Character),
		npcs:         make(map[string]*Character),
		chat:          NewChat(),
		lastUpdate:    time.Now(),
		wiSpriteCache: make(map[string]*ebiten.Image),
	}

	// Camera zoom
	g.zoom = 1.0
	g.worldBuf = ebiten.NewImage(worldBufW, worldBufH)

	// GMAP chunk manager (always created; activated when loadGMap is called)
	g.chunkMgr = NewChunkManager()

	// Load spawn map (TMX or GMAP) from config.
	if strings.HasSuffix(strings.ToLower(Cfg.SpawnMap), ".gmap") {
		g.loadGMap(Cfg.SpawnMap)
	} else {
		g.loadMap(Cfg.SpawnMap, false)
	}

	// Panel menu
	g.panelMenu = NewPanelMenu()

	// Social panel (Friends / Guilds / Quests)
	g.socialPanel = NewSocialPanel()

	// Emoji images
	loadEmojiImages()

	// Load gralat sprites
	loadGralatImage()

	// Offscreen image for character previews (reused each frame).
	g.previewImg = ebiten.NewImage(96, 96)

	// Pre-render HP circle sprites (full, half, empty).
	g.hpCircles = makeHPCircles()

	// Tap-to-buy icon (shown above priced world items when nearby)
	g.tapToBuyImg, _, _ = ebitenutil.NewImageFromFile(
		"assets/offline/levels/images/dcicon/dcicon_taptobuy0.png")

	// Virtual action button images
	g.grabKeyImg, _, _ = ebitenutil.NewImageFromFile(
		"assets/offline/levels/images/classiciphone/classiciphone_virtualkey_glove_blue.png")
	g.swordKeyImg, _, _ = ebitenutil.NewImageFromFile(
		"assets/offline/levels/images/classiciphone/classiciphone_virtualkey_sword_blue.png")

	return g
}

// ──────────────────────────────────────────────────────────────
// Ebiten interface
// ──────────────────────────────────────────────────────────────

func (g *Game) Update() error {
	now := time.Now()
	dt := now.Sub(g.lastUpdate).Seconds()
	g.lastUpdate = now
	if dt > 0.05 {
		dt = 0.05
	}

	switch g.state {
	case StateMainMenu:
		ns := g.mainMenu.Update()
		if g.mainMenu.WantsQuit() {
			return ebiten.Termination
		}
		if ns != StateMainMenu {
			g.state = ns
		}

	case StateLogin:
		ns := g.loginMenu.Update()
		if ns == StatePlaying {
			g.startGame(g.loginMenu.Token, g.loginMenu.Name)
			g.state = StatePlaying
		} else if ns != StateLogin {
			g.state = ns
		}

	case StateRegister:
		ns := g.regMenu.Update()
		if ns == StatePlaying {
			g.startGame(g.regMenu.Token, g.regMenu.Name)
			g.state = StatePlaying
		} else if ns != StateRegister {
			g.state = ns
		}

	case StatePlaying:
		// Must run before updatePlaying: entity dead reckoning clamps against
		// these, and in GMAP mode the real world size only arrives asynchronously.
		g.refreshWorldBounds()
		if err := g.updatePlaying(dt); err != nil {
			return err
		}
	}
	return nil
}

func (g *Game) Draw(screen *ebiten.Image) {
	switch g.state {
	case StateMainMenu:
		g.mainMenu.Draw(screen)
	case StateLogin:
		g.loginMenu.Draw(screen)
	case StateRegister:
		g.regMenu.Draw(screen)
	case StatePlaying:
		g.drawPlaying(screen)
		g.cosmeticMenu.Draw(screen)
	}
}

func (g *Game) Layout(_, _ int) (int, int) {
	return screenW, screenH
}

// ──────────────────────────────────────────────────────────────
// Map helpers
// ──────────────────────────────────────────────────────────────

func (g *Game) worldSize() (int, int) {
	if g.activeGMap != "" && g.chunkMgr.HasGMap() {
		return g.chunkMgr.WorldW(), g.chunkMgr.WorldH()
	}
	if g.gameMap != nil {
		return g.gameMap.WorldW(), g.gameMap.WorldH()
	}
	return worldW, worldH
}

// activeWorldW/H hold the pixel size of the world currently loaded.
//
// Character.Update clamps dead-reckoned positions against them and has no access
// to the Game, so they live at package level. They must be refreshed every frame
// rather than once at map load: in GMAP mode the real size only becomes known
// after the metadata request completes.
//
// Leaving them at the compile-time TMX constants (1120 px) is not a harmless
// approximation — on a larger world every remote entity's predicted position was
// clamped back to ~1088 px each frame, tripping the "large desync" snap and
// making NPCs and other players flicker between their real position and the
// top-left corner of the world.
var activeWorldW, activeWorldH = float64(worldW), float64(worldH)

func (g *Game) refreshWorldBounds() {
	if w, h := g.worldSize(); w > 0 && h > 0 {
		activeWorldW, activeWorldH = float64(w), float64(h)
	}
}

// terrainAt returns the terrain ("water", "lava", or "") at the given rect.
func (g *Game) terrainAt(x, y, w, h float64) string {
	if g.activeGMap != "" {
		return g.chunkMgr.TerrainAt(x, y, w, h)
	}
	if g.gameMap != nil {
		return g.gameMap.TerrainAt(x, y, w, h)
	}
	return ""
}

func (g *Game) loadGMap(name string) {
	if len(name) < 5 || name[len(name)-5:] != ".gmap" {
		name += ".gmap"
	}
	g.activeGMap = name
	g.gameMap = nil // switch off TMX map
	g.chunkMgr = NewChunkManager()
	g.chunkMgr.LoadGMap(name)
	g.mapSwitchCooldown = 1.0
	if g.conn != nil {
		g.conn.SendJSON(map[string]string{"type": "change_map", "map": name})
	}
	if g.localChar != nil {
		// Spawn near origin; exact world size is not yet known
		g.localChar.X = float64(chunkPixelW)/2 - float64(frameW)/2
		g.localChar.Y = float64(chunkPixelH)/2 - float64(frameH)/2
	}
}

func (g *Game) loadMap(name string, spawnAtExit bool) {
	filename := name
	lower := strings.ToLower(filename)
	if strings.HasSuffix(lower, ".nw") {
		filename = filename[:len(filename)-3] + ".tmx"
	} else if !strings.HasSuffix(lower, ".tmx") {
		filename += ".tmx"
	}
	// If the name has no directory component, probe both candidate directories.
	// Use ReadGameFile (works on native + WASM) to avoid os.Stat which is
	// broken in WASM. maps/tmx/ takes priority (NW-converted chunk files);
	// maps/ is the fallback for hand-authored TMX maps.
	if !strings.Contains(filename, "/") {
		primary := "maps/tmx/" + filename
		if d, err := ReadGameFile(primary); err == nil && len(d) > 0 {
			filename = primary
		} else {
			filename = "maps/" + filename
		}
	}
	gm, err := LoadTMX(filename)
	if err != nil {
		fmt.Println("[MAP] failed to load", filename, ":", err)
		return
	}
	g.prevMapName = g.currentMapName
	g.currentMapName = filename
	g.gameMap = gm
	// Disable grass fill for interior/building maps.
	// A map loaded while prevGMap is set is always a building interior.
	// Maps not in maps/tmx/ are also hand-authored interiors.
	gm.NoGrassFill = g.prevGMap != "" || !strings.HasPrefix(filename, "maps/tmx/")
	g.mapSwitchCooldown = 1.0

	if g.conn != nil {
		g.conn.SendJSON(map[string]string{"type": "change_map", "map": filename})
	}

	// Fetch server-side NW data (links, signs) asynchronously.
	// The TMX file on disk has no switchmap/panneau layers, so we supplement
	// from the server's chunk API which reads the companion .nw file.
	go func(m *GameMap, fname string) {
		data, err := fetchChunk(fname)
		if err != nil {
			fmt.Printf("[MAP] server data fetch %s: %v\n", fname, err)
			return
		}
		m.SetServerData(data.Links, data.Signs)
		fmt.Printf("[MAP] loaded %d links, %d signs from server for %s\n",
			len(data.Links), len(data.Signs), fname)
	}(gm, filename)

	if g.localChar == nil {
		return
	}
	ww, wh := g.worldSize()
	if spawnAtExit {
		if ex, ey, ok := gm.ExitPos(); ok {
			g.localChar.X = ex - float64(frameW)/2
			g.localChar.Y = ey - float64(frameH)/2
			return
		}
	}
	g.localChar.X = float64(ww)/2 - float64(frameW)/2
	g.localChar.Y = float64(wh)/2 - float64(frameH)/2
}

// ──────────────────────────────────────────────────────────────
// Game start / stop
// ──────────────────────────────────────────────────────────────

func (g *Game) startGame(token, name string) {
	g.localName = name
	g.localID = ""
	g.localGralats = 0
	g.localInventory = nil
	g.isAdmin = false
	g.signDialog = ""
	g.npcDialog = ""
	g.profileOpen = false
	g.viewedPlayer = nil
	g.nearWorldItem = nil
	g.mu.Lock()
	g.otherPlayers = make(map[string]*Character)
	g.npcs = make(map[string]*Character)
	g.worldItems = nil
	g.mu.Unlock()
	g.grMu.Lock()
	g.worldGralats = nil
	g.grMu.Unlock()
	if g.inventoryMenu != nil {
		g.inventoryMenu.Close()
	}
	if g.adminMenu != nil {
		g.adminMenu.Close()
	}

	ww, wh := g.worldSize()
	spawnX := float64(ww)/2 - float64(frameW)/2
	spawnY := float64(wh)/2 - float64(frameH)/2
	if Cfg.SpawnX != 0 || Cfg.SpawnY != 0 {
		spawnX = Cfg.SpawnX
		spawnY = Cfg.SpawnY
	}
	g.localChar = NewCharacter(g.bodyImg, g.headImg,
		spawnX, spawnY,
		name, false, 0)
	g.localChar.IsLocal = true

	g.localHP = 6
	g.localMaxHP = 6

	g.chat = NewChat()
	g.chat.AddMessage("", "Welcome! [WASD/ZQSD] move  [X] sword  [A] grab  [R] mount  [F] interact  [P] profile", true)

	go func() {
		conn, err := Dial(getWSURL())
		if err != nil {
			g.chat.AddMessage("", "Connection error: "+err.Error(), true)
			return
		}
		g.conn = conn
		conn.SendJSON(map[string]string{"type": "auth", "token": token})
	}()
}

// restorePrevGMap returns to the GMAP that was active before entering a building.
// Restores the saved ChunkManager so all previously loaded chunks are immediately available.
func (g *Game) restorePrevGMap() {
	savedX := g.prevGMapX
	savedY := g.prevGMapY
	g.activeGMap = g.prevGMap
	g.gameMap = nil
	if g.prevChunkMgr != nil {
		g.chunkMgr = g.prevChunkMgr
	}
	g.prevGMap = ""
	g.prevChunkMgr = nil
	g.mapSwitchCooldown = 1.0
	if g.conn != nil {
		g.conn.SendJSON(map[string]string{"type": "change_map", "map": g.activeGMap})
	}
	if g.localChar != nil {
		g.localChar.X = savedX
		g.localChar.Y = savedY
		g.localChar.TargetX = savedX
		g.localChar.TargetY = savedY
	}
}

func (g *Game) disconnect() {
	if g.conn != nil {
		g.conn.Close()
		g.conn = nil
	}
}

func (g *Game) sendCosmetics() {
	if g.conn == nil {
		return
	}
	cm := g.cosmeticMenu
	if g.localChar != nil {
		g.localChar.SetCosmetics(cm.BodyFile(), cm.HeadFile(), cm.HatFile(), cm.ShieldFile(), cm.SwordFile())
	}
	g.conn.SendJSON(map[string]string{
		"type":   "cosmetic",
		"body":   cm.BodyFile(),
		"head":   cm.HeadFile(),
		"hat":    cm.HatFile(),
		"shield": cm.ShieldFile(),
		"sword":  cm.SwordFile(),
	})
}

// ──────────────────────────────────────────────────────────────
// Asset loader (called from main.go)
// ──────────────────────────────────────────────────────────────

func loadAssets() (body, head, tiles *ebiten.Image) {
	var err error
	from := func(path string) *ebiten.Image {
		img, _, e := ebitenutil.NewImageFromFile(path)
		err = e
		if e != nil {
			fmt.Println("[ASSETS]", path, "not found, fallback rendering active")
		}
		return img
	}
	body = from("assets/sprites/character.png")
	head = from("assets/sprites/head.png")
	tiles = from("assets/sprites/tiles.png")
	_ = err
	return
}

