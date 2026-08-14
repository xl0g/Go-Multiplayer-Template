package main

import (
	"darkzone/MultiTestServer/internal/db"
	"encoding/json"
	"fmt"
	"log"
	"math"
	mrand "math/rand"
	"sync"
	"time"
)

// Hub manages all connected clients and drives the server-side game loop.
type Hub struct {
	mu      sync.RWMutex
	clients map[*Client]bool
	npcs    []*NPC

	// World collision — either a CollisionMap (TMX) or a GMapWorld (GMAP).
	worldColl        WorldCollider
	worldW, worldH   float64 // cached from worldColl.Bounds(); never changes after init

	// World gralat pickups
	gralats     []*GralatPickup
	gralatTimer map[string]time.Time    // id → scheduled respawn time
	gralatSpawn map[string]GralatPickup // id → resolved (wall-free) spawn definition

	// Admin-spawned world items (persisted in world_items table)
	worldItems []*WorldSpawnItem
}

var globalHub *Hub

// mapOrDefault normalizes an empty map ID to the default map. Every map-instance
// comparison must go through this so "" and defaultMap are never treated as
// two different instances.
func mapOrDefault(m string) string {
	if m == "" {
		return defaultMap
	}
	return m
}

// findNPCOnMap returns the NPC with the given ID if — and only if — it lives on
// mapID. Interaction handlers must use this instead of a bare ID lookup:
// coordinate spaces are per-map, so an ID-only match lets a player on one map
// reach an NPC standing at the same coordinates on another.
// Caller must hold h.mu (read or write).
func (h *Hub) findNPCOnMap(npcID, mapID string) *NPC {
	for _, n := range h.npcs {
		if n.state.ID == npcID && mapOrDefault(n.mapID) == mapID {
			return n
		}
	}
	return nil
}

func newHub() *Hub {
	h := &Hub{
		clients:     make(map[*Client]bool),
		gralatTimer: make(map[string]time.Time),
		gralatSpawn: make(map[string]GralatPickup),
	}

	// Load world collision from the same spawn map as the client (config.json → spawnMap).
	h.worldColl = loadWorldCollider("config.json")

	npcDefs := []struct {
		name    string
		x, y    float64
		npcType int
	}{
		// Regular NPCs
		{"Thibaut the Villager", 300, 200, NPCTypeVillager},
		{"Marceline the Merchant", 600, 280, NPCTypeMerchant},
		{"Eleanor the Traveller", 420, 480, NPCTypeTraveler},
		{"Baptiste the Farmer", 680, 520, NPCTypeFarmer},
		// Passive animals (flee from players)
		{"Lapin", 350, 350, NPCTypePassive},
		{"Biche", 750, 400, NPCTypePassive},
		{"Poulet", 500, 600, NPCTypePassive},
		// Aggressive monsters
		{"Slime Rouge", 200, 700, NPCTypeAggressive},
		{"Slime Vert", 800, 650, NPCTypeAggressive},
		{"Gobelin", 550, 800, NPCTypeAggressive},
		{"Bat", 900, 300, NPCTypeAggressive},
		// Rideable horses (mount system)
		{"Épona", 480, 360, NPCTypeHorse},
		{"Bourrin", 640, 700, NPCTypeHorse},
	}

	if h.worldColl != nil {
		h.worldW, h.worldH = h.worldColl.Bounds()
	} else {
		h.worldW, h.worldH = mapWidth, mapHeight
	}

	for i, def := range npcDefs {
		x, y := def.x, def.y
		if h.worldColl != nil && !h.worldColl.IsFreePoint(x+8, y+8) {
			x, y = findFreePos(h.worldColl, x, y, h.worldW, h.worldH)
		}
		n := newNPC(fmt.Sprintf("npc_%d", i), def.name, x, y, def.npcType)
		n.worldW, n.worldH = h.worldW, h.worldH
		h.npcs = append(h.npcs, n)
	}

	// Load persisted world items from DB.
	for _, w := range db.LoadWorldItems() {
		h.worldItems = append(h.worldItems, &WorldSpawnItem{
			ID: w.ID, Name: w.Name, SpritePath: w.SpritePath,
			X: w.X, Y: w.Y, Price: w.Price, ItemID: w.ItemID,
			MapID: mapOrDefault(w.MapName),
		})
	}

	// Resolve gralat spawn positions once — respawns reuse these so a coin
	// never reappears inside a wall.
	for i := range gralatSpawnDefs {
		d := gralatSpawnDefs[i]
		x, y := d.x, d.y
		if h.worldColl != nil && !h.worldColl.IsFreePoint(x+8, y+8) {
			x, y = findFreePos(h.worldColl, x, y, h.worldW, h.worldH)
		}
		g := GralatPickup{ID: d.id, X: x, Y: y, Value: d.value, MapID: defaultMap}
		h.gralatSpawn[d.id] = g
		gc := g
		h.gralats = append(h.gralats, &gc)
	}

	return h
}

// register adds a client to the hub.
func (h *Hub) register(c *Client) {
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
	h.broadcastSystem(fmt.Sprintf("%s joined the world!", c.name))
	log.Printf("[HUB] %s connected (ID: %s)", c.name, c.playerID)
}

// unregister removes a client, frees any mount, saves position and playtime.
func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	// Free any horse this player was riding
	for _, n := range h.npcs {
		if n.mountedBy == c.playerID {
			n.mountedBy = ""
			n.state.MountedBy = ""
			break
		}
	}
	// Capture the position and the map it belongs to under the same lock.
	lastX, lastY, lastMap := c.state.X, c.state.Y, mapOrDefault(c.currentMap)
	h.mu.Unlock()
	elapsed := int(time.Since(c.sessionStart).Seconds())
	db.UpdatePosition(c.userID, lastX, lastY, lastMap)
	db.AddPlaytime(c.userID, elapsed)
	h.broadcastSystem(fmt.Sprintf("%s left the world.", c.name))
	log.Printf("[HUB] %s disconnected (session: %ds)", c.name, elapsed)
}

func (h *Hub) broadcastRaw(data []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- data:
		default:
		}
	}
}

func (h *Hub) broadcast(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.broadcastRaw(data)
}

func (h *Hub) broadcastSystem(msg string) {
	h.broadcast(map[string]string{"type": "system", "msg": msg})
}

// sendPerClientState sends each client a state snapshot filtered by:
//  1. Same map as the receiver.
//  2. Within viewRadius world-px of the receiver (interest management).
//
// A tempGrid is rebuilt each tick — O(n) to construct, O(1) per cell lookup.
func (h *Hub) sendPerClientState() {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// Build a flat snapshot of every online player.
	type playerSnap struct {
		state PlayerState
		mapID string
	}
	snaps := make([]playerSnap, 0, len(h.clients))
	for c := range h.clients {
		ps := c.state
		ps.Playtime = c.savedPlaytime + int(time.Since(c.sessionStart).Seconds())
		snaps = append(snaps, playerSnap{ps, mapOrDefault(c.currentMap)})
	}

	// Build spatial grid from snapshot positions.
	xs := make([]float64, len(snaps))
	ys := make([]float64, len(snaps))
	for i, s := range snaps {
		xs[i] = s.state.X
		ys[i] = s.state.Y
	}
	grid := buildSpatialGrid(xs, ys)

	// Snapshot NPCs grouped by map.
	npcsByMap := make(map[string][]NPCState)
	for _, n := range h.npcs {
		if n.combat.IsAlive() || n.combat.RecentlyDied() {
			mid := mapOrDefault(n.mapID)
			npcsByMap[mid] = append(npcsByMap[mid], n.state)
		}
	}

	// Snapshot gralats / world items grouped by map instance.
	gralatsByMap := make(map[string][]GralatPickup)
	for _, g := range h.gralats {
		mid := mapOrDefault(g.MapID)
		gralatsByMap[mid] = append(gralatsByMap[mid], *g)
	}
	itemsByMap := make(map[string][]WorldSpawnItem)
	for _, wi := range h.worldItems {
		mid := mapOrDefault(wi.MapID)
		itemsByMap[mid] = append(itemsByMap[mid], *wi)
	}

	radiusSq := viewRadius * viewRadius

	for c := range h.clients {
		myMap := mapOrDefault(c.currentMap)
		cx, cy := c.state.X, c.state.Y

		// Nearby players: same map + within viewRadius.
		nearIdx := grid.nearby(cx, cy)
		players := make([]PlayerState, 0, len(nearIdx))
		for _, idx := range nearIdx {
			if snaps[idx].mapID == myMap {
				players = append(players, snaps[idx].state)
			}
		}

		// NPCs: same map, radius-filtered.
		var sendNPCs []NPCState
		for _, n := range npcsByMap[myMap] {
			dx, dy := n.X-cx, n.Y-cy
			if dx*dx+dy*dy <= radiusSq {
				sendNPCs = append(sendNPCs, n)
			}
		}

		// Gralats / world items: same map, radius-filtered.
		var sendGralats []GralatPickup
		var sendWorldItems []WorldSpawnItem
		for _, g := range gralatsByMap[myMap] {
			dx, dy := g.X-cx, g.Y-cy
			if dx*dx+dy*dy <= radiusSq {
				sendGralats = append(sendGralats, g)
			}
		}
		for _, wi := range itemsByMap[myMap] {
			dx, dy := wi.X-cx, wi.Y-cy
			if dx*dx+dy*dy <= radiusSq {
				sendWorldItems = append(sendWorldItems, wi)
			}
		}

		data, err := json.Marshal(map[string]interface{}{
			"type":        "state",
			"players":     players,
			"npcs":        sendNPCs,
			"gralats":     sendGralats,
			"world_items": sendWorldItems,
		})
		if err != nil {
			continue
		}
		select {
		case c.send <- data:
		default:
		}
	}
}

// ──────────────────────────────────────────────────────────────
// Combat
// ──────────────────────────────────────────────────────────────

// damageNPC reduces the HP of the NPC npcID on mapID by dmg.
// Returns (newHP, killed). Returns (-1, false) if the NPC is not on that map,
// is immune, or is on cooldown.
func (h *Hub) damageNPC(npcID, mapID string, dmg int) (newHP int, killed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.findNPCOnMap(npcID, mapID)
	if n == nil {
		return -1, false
	}
	newHP, killed = n.combat.Damage(dmg)
	if newHP >= 0 {
		n.syncState()
	}
	return newHP, killed
}

// ──────────────────────────────────────────────────────────────
// Mount
// ──────────────────────────────────────────────────────────────

// mountNPC marks the horse npcID on mapID as ridden by playerID.
// Returns false if the horse isn't on that map, doesn't exist, is already
// ridden, or is the wrong type.
func (h *Hub) mountNPC(npcID, mapID, playerID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.findNPCOnMap(npcID, mapID)
	if n == nil || n.state.NPCType != NPCTypeHorse ||
		n.mountedBy != "" || !n.combat.IsAlive() {
		return false
	}
	n.mountedBy = playerID
	n.state.MountedBy = playerID
	return true
}

// dismountNPC frees the horse currently ridden by playerID.
func (h *Hub) dismountNPC(playerID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, n := range h.npcs {
		if n.mountedBy == playerID {
			n.mountedBy = ""
			n.state.MountedBy = ""
			return
		}
	}
}

// updateHorsePos moves the horse ridden by playerID to (x, y).
// Must be called while holding h.mu write lock.
func (h *Hub) updateHorsePos(playerID string, x, y float64) {
	for _, n := range h.npcs {
		if n.mountedBy == playerID {
			n.state.X = x
			n.state.Y = y
			return
		}
	}
}

// ──────────────────────────────────────────────────────────────
// Gralat respawn
// ──────────────────────────────────────────────────────────────

// collectGralat removes the pickup id and returns its value, but only if the
// collector is on the same map and standing within gralatReach of it. Without
// those checks any client could drain every coin in the world from anywhere.
// Returns 0 when the pickup is gone, on another map, or out of reach.
func (h *Hub) collectGralat(id, mapID string, px, py float64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, g := range h.gralats {
		if g.ID != id {
			continue
		}
		if mapOrDefault(g.MapID) != mapID {
			return 0
		}
		dx, dy := g.X-px, g.Y-py
		if dx*dx+dy*dy > gralatReach*gralatReach {
			return 0
		}
		value := g.Value
		h.gralats = append(h.gralats[:i], h.gralats[i+1:]...)
		h.gralatTimer[id] = time.Now().Add(respawnDelay)
		return value
	}
	return 0
}

func (h *Hub) checkRespawns() {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, t := range h.gralatTimer {
		if !now.After(t) {
			continue
		}
		// Respawn from the resolved spawn position, not the raw definition —
		// the raw one may sit inside a wall.
		if spawn, ok := h.gralatSpawn[id]; ok {
			g := spawn
			h.gralats = append(h.gralats, &g)
		}
		delete(h.gralatTimer, id)
	}
}

// ──────────────────────────────────────────────────────────────
// World items (admin-spawned)
// ──────────────────────────────────────────────────────────────

func (h *Hub) addWorldItem(wi *WorldSpawnItem) {
	h.mu.Lock()
	h.worldItems = append(h.worldItems, wi)
	h.mu.Unlock()
}

func (h *Hub) removeWorldItem(id string) {
	h.mu.Lock()
	for i, wi := range h.worldItems {
		if wi.ID == id {
			h.worldItems = append(h.worldItems[:i], h.worldItems[i+1:]...)
			break
		}
	}
	h.mu.Unlock()
}

// ──────────────────────────────────────────────────────────────
// Game loop
// ──────────────────────────────────────────────────────────────

func (h *Hub) runGameLoop() {
	ticker := time.NewTicker(time.Second / 60)
	defer ticker.Stop()
	lastTime := time.Now()
	respawnTick := 0

	for range ticker.C {
		now := time.Now()
		dt := now.Sub(lastTime).Seconds()
		lastTime = now
		if dt > 0.1 {
			dt = 0.1
		}

		h.mu.Lock()
		// Build player snapshots per map for NPC AI.
		playersByMap := make(map[string][]playerPos, 4)
		for c := range h.clients {
			cm := mapOrDefault(c.currentMap)
			playersByMap[cm] = append(playersByMap[cm], playerPos{
				id:    c.playerID,
				x:     c.state.X,
				y:     c.state.Y,
				alive: c.state.HP > 0,
			})
		}

		// Update NPCs; each NPC only sees players on its own map.
		type npcAttack struct {
			playerID string
			mapID    string
		}
		var attacks []npcAttack
		for _, n := range h.npcs {
			mid := mapOrDefault(n.mapID)
			if attackedID := n.update(dt, h.worldColl, playersByMap[mid]); attackedID != "" {
				attacks = append(attacks, npcAttack{playerID: attackedID, mapID: mid})
			}
		}
		// Remove noRespawn NPCs that have died.
		for i := len(h.npcs) - 1; i >= 0; i-- {
			n := h.npcs[i]
			if !n.combat.IsAlive() && n.combat.noRespawn {
				h.npcs = append(h.npcs[:i], h.npcs[i+1:]...)
			}
		}

		// Apply NPC attacks using the shared CombatEntity — same rules as PvP.
		for _, atk := range attacks {
			for c := range h.clients {
				if c.playerID != atk.playerID || mapOrDefault(c.currentMap) != atk.mapID {
					continue
				}
				newHP, killed := c.combat.Damage(aggroDamage)
				if newHP < 0 {
					break // immune (hit cooldown)
				}
				c.state.HP = newHP
				msg := map[string]interface{}{"type": "pvp_damage", "hp": newHP}
				if killed {
					c.state.AnimState = "dead"
					msg["killed"] = true
				}
				data, _ := json.Marshal(msg)
				select {
				case c.send <- data:
				default:
				}
				break
			}
		}
		h.mu.Unlock()

		// Tick Lua resources (timers + queued events).
		if globalLuaManager != nil {
			globalLuaManager.Tick(dt)
		}

		respawnTick++
		if respawnTick >= 300 {
			respawnTick = 0
			h.checkRespawns()
		}

		h.sendPerClientState()
	}
}

// ── Spawned enemy (admin command) ─────────────────────────────────────────────

// spawnEnemyAt creates a temporary aggressive NPC near (x, y) on mapID.
// The enemy has 6 HP, moves at spawnedEnemySpeed, and is permanently removed on death.
func (h *Hub) spawnEnemyAt(name, mapID string, x, y float64) {
	id := fmt.Sprintf("enemy_%d", time.Now().UnixNano())

	// Spawn 100 px away in a random direction at a free (non-wall) position.
	angle := mrand.Float64() * 2 * math.Pi
	sx := x + math.Cos(angle)*100
	sy := y + math.Sin(angle)*100
	if h.worldColl != nil {
		sx, sy = findFreePos(h.worldColl, sx, sy, h.worldW, h.worldH)
	}
	sx = clamp(sx, 0, h.worldW-32)
	sy = clamp(sy, 0, h.worldH-32)

	npc := newNPC(id, name, sx, sy, NPCTypeSpawnedEnemy)
	npc.mapID = mapID
	npc.combat.noRespawn = true
	npc.worldW, npc.worldH = h.worldW, h.worldH
	npc.combat.atkCD = aggroAttackCD // no immediate first strike

	h.mu.Lock()
	h.npcs = append(h.npcs, npc)
	h.mu.Unlock()
}

// ── Lua NPC helpers ──────────────────────────────────────────────────────────

// addLuaNPC adds a Lua-spawned NPC to the hub.
func (h *Hub) addLuaNPC(npc *NPC) {
	h.mu.Lock()
	// newNPC defaults to the 1120px TMX bounds; adopt the real world size so
	// Lua NPCs are not clamped into the top-left corner of a larger GMAP world.
	npc.worldW, npc.worldH = h.worldW, h.worldH
	h.npcs = append(h.npcs, npc)
	h.mu.Unlock()
}

// removeLuaNPC removes an NPC by ID (used when a resource stops).
func (h *Hub) removeLuaNPC(id string) {
	h.mu.Lock()
	for i, n := range h.npcs {
		if n.state.ID == id {
			h.npcs = append(h.npcs[:i], h.npcs[i+1:]...)
			break
		}
	}
	h.mu.Unlock()
}

// setLuaNPCPos teleports an NPC to a new position.
func (h *Hub) setLuaNPCPos(id string, x, y float64) {
	h.mu.Lock()
	for _, n := range h.npcs {
		if n.state.ID == id {
			n.state.X = x
			n.state.Y = y
			n.homeX = x
			n.homeY = y
			n.targetX = x
			n.targetY = y
			break
		}
	}
	h.mu.Unlock()
}

// setLuaNPCDialog sets a custom dialog for an NPC.
func (h *Hub) setLuaNPCDialog(id, msg string, gMin, gMax int) {
	h.mu.Lock()
	for _, n := range h.npcs {
		if n.state.ID == id {
			n.customDialog = msg
			n.customGMin = gMin
			n.customGMax = gMax
			break
		}
	}
	h.mu.Unlock()
}

// getLuaNPCPos returns the current position of an NPC.
func (h *Hub) getLuaNPCPos(id string) (x, y float64, ok bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, n := range h.npcs {
		if n.state.ID == id {
			return n.state.X, n.state.Y, true
		}
	}
	return 0, 0, false
}
