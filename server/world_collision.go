package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// serverConfig mirrors the client-side config.json fields the server cares about.
type serverConfig struct {
	SpawnMap string `json:"spawnMap"`
}

// resolveDefaultMap derives the default map ID from config.json, using exactly
// the same naming the client announces in its change_map messages
// (Game.activeGMap for GMAP mode, Game.currentMapName for TMX mode).
//
// This has to agree with the client. The server groups every entity with an
// empty mapID onto this ID, so if it disagrees, all built-in NPCs, gralats and
// world items end up on a map instance no player is ever on — invisible and
// unreachable, while the game otherwise looks healthy.
func resolveDefaultMap(configPath string) string {
	const fallback = "maps/GraalRebornMap.tmx"
	cfg := serverConfig{SpawnMap: fallback}
	if data, err := os.ReadFile(configPath); err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	name := strings.TrimSpace(cfg.SpawnMap)
	if name == "" {
		return fallback
	}

	// GMAP mode: the client keeps the configured name as-is (appending .gmap if
	// absent) and announces that.
	if strings.HasSuffix(strings.ToLower(name), ".gmap") {
		return name
	}

	// TMX mode: mirror the client's loadMap() resolution — normalize the suffix,
	// then probe maps/tmx/ before maps/ for bare filenames.
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".nw") {
		name = name[:len(name)-3] + ".tmx"
	} else if !strings.HasSuffix(lower, ".tmx") {
		name += ".tmx"
	}
	if !strings.Contains(name, "/") {
		primary := filepath.Join("maps", "tmx", name)
		if st, err := os.Stat(primary); err == nil && !st.IsDir() {
			return primary
		}
		return filepath.Join("maps", name)
	}
	return name
}

// loadWorldCollider reads config.json to determine the spawn map type,
// then returns the appropriate WorldCollider (GMapWorld or CollisionMap).
func loadWorldCollider(configPath string) WorldCollider {
	cfg := serverConfig{SpawnMap: "maps/GraalRebornMap.tmx"}
	if data, err := os.ReadFile(configPath); err == nil {
		_ = json.Unmarshal(data, &cfg)
	}

	spawnMap := cfg.SpawnMap
	lower := strings.ToLower(spawnMap)

	switch {
	case strings.HasSuffix(lower, ".gmap"):
		// Resolve: bare name → maps/gmap/name.gmap
		gmapPath := spawnMap
		if !strings.Contains(gmapPath, "/") {
			gmapPath = filepath.Join("maps/gmap", gmapPath)
		}
		if !strings.HasSuffix(strings.ToLower(gmapPath), ".gmap") {
			gmapPath += ".gmap"
		}
		gw, err := NewGMapWorld(gmapPath)
		if err != nil {
			log.Printf("[COLL] Could not load GMAP collision %q: %v — NPCs will ignore walls", gmapPath, err)
			return nil
		}
		w, h := gw.Bounds()
		log.Printf("[COLL] GMapWorld loaded: %s (%.0f×%.0f px)", gmapPath, w, h)
		return gw

	default: // .tmx
		tmxPath := spawnMap
		if !strings.Contains(tmxPath, "/") {
			tmxPath = filepath.Join("maps", tmxPath)
		}
		cm, err := LoadCollisionMap(tmxPath)
		if err != nil {
			log.Printf("[COLL] Could not load TMX collision %q: %v — NPCs will ignore walls", tmxPath, err)
			return nil
		}
		w, h := cm.Bounds()
		log.Printf("[COLL] CollisionMap loaded: %s (%.0f×%.0f px)", tmxPath, w, h)
		return cm
	}
}

// chunkPxSize is the pixel size of one NW/TMX chunk (64 tiles × 16 px).
const chunkPxSize = NWCols * 16 // 1024

// WorldCollider is the unified collision interface used by NPC AI.
// Both TMX maps and GMAP worlds implement it.
type WorldCollider interface {
	IsBlocked(x, y, w, h float64) bool
	IsFreePoint(x, y float64) bool
	Bounds() (worldW, worldH float64)
}

// ── CollisionMap satisfies WorldCollider ──────────────────────────────────────

func (cm *CollisionMap) Bounds() (float64, float64) {
	if cm == nil {
		return float64(mapWidth), float64(mapHeight)
	}
	return float64(cm.cols * cm.tileW), float64(cm.rows * cm.tileH)
}

// ── GMapWorld: lazy chunk-based collision for large GMAP worlds ───────────────

// GMapWorld answers collision queries for a GMAP by lazily loading per-chunk
// collision grids from NW/TMX files. Safe for concurrent read after init.
type GMapWorld struct {
	gm     *GMap
	mu     sync.RWMutex
	chunks map[[2]int][][]bool // [gridCol,gridRow] → 64×64 solid grid (nil = empty/unknown)
}

// NewGMapWorld parses the .gmap file at gmapPath and returns a GMapWorld.
func NewGMapWorld(gmapPath string) (*GMapWorld, error) {
	gm, err := ParseGMapFile(gmapPath)
	if err != nil {
		return nil, err
	}
	return &GMapWorld{
		gm:     gm,
		chunks: make(map[[2]int][][]bool),
	}, nil
}

func (w *GMapWorld) Bounds() (float64, float64) {
	return float64(w.gm.Width * chunkPxSize), float64(w.gm.Height * chunkPxSize)
}

// chunkCoords converts a world-pixel point to (gridCol, gridRow, localTileCol, localTileRow).
func (w *GMapWorld) chunkCoords(px, py float64) (gc, gr, lc, lr int) {
	gc = int(px) / chunkPxSize
	gr = int(py) / chunkPxSize
	lc = (int(px) % chunkPxSize) / 16
	lr = (int(py) % chunkPxSize) / 16
	return
}

// loadChunk reads collision data for chunk (gc, gr) from disk.
// Returns nil if the chunk doesn't exist or has no collision layer.
func (w *GMapWorld) loadChunk(gc, gr int) [][]bool {
	if gr < 0 || gr >= len(w.gm.LevelNames) {
		return nil
	}
	row := w.gm.LevelNames[gr]
	if gc < 0 || gc >= len(row) {
		return nil
	}
	name := row[gc]
	if name == "" {
		return nil
	}
	base := strings.TrimSuffix(name, filepath.Ext(name))

	// Try NW file first (has direct Collision [][]bool).
	if lv, err := ParseNWFile(filepath.Join(mapsNWDir, base+".nw"), 0); err == nil {
		return lv.Collision
	}

	// Fall back to TMX.
	resp, err := ParseTMXFile(filepath.Join(mapsTMXDir, base+".tmx"))
	if err != nil {
		return nil
	}
	for _, layer := range resp.Layers {
		if !layer.Collision || len(layer.Data) != NWCols*NWRows {
			continue
		}
		grid := make([][]bool, NWRows)
		for r := 0; r < NWRows; r++ {
			grid[r] = make([]bool, NWCols)
			for c := 0; c < NWCols; c++ {
				grid[r][c] = layer.Data[r*NWCols+c] != 0
			}
		}
		return grid
	}
	return nil
}

// chunk returns the cached collision grid for (gc, gr), loading it if needed.
func (w *GMapWorld) chunk(gc, gr int) [][]bool {
	key := [2]int{gc, gr}
	w.mu.RLock()
	grid, ok := w.chunks[key]
	w.mu.RUnlock()
	if ok {
		return grid
	}
	grid = w.loadChunk(gc, gr)
	w.mu.Lock()
	w.chunks[key] = grid // store nil too so we don't retry every tick
	w.mu.Unlock()
	return grid
}

func (w *GMapWorld) isPointSolid(px, py float64) bool {
	gc, gr, lc, lr := w.chunkCoords(px, py)
	grid := w.chunk(gc, gr)
	if grid == nil || lr < 0 || lr >= len(grid) || lc < 0 || lc >= len(grid[lr]) {
		return false
	}
	return grid[lr][lc]
}

func (w *GMapWorld) IsFreePoint(x, y float64) bool {
	return !w.isPointSolid(x, y)
}

// IsBlocked checks the four inner corners of the bounding box (same logic as CollisionMap).
func (w *GMapWorld) IsBlocked(x, y, bw, bh float64) bool {
	const margin = 2.0
	return w.isPointSolid(x+margin, y+margin) ||
		w.isPointSolid(x+bw-margin, y+margin) ||
		w.isPointSolid(x+margin, y+bh-margin) ||
		w.isPointSolid(x+bw-margin, y+bh-margin)
}
