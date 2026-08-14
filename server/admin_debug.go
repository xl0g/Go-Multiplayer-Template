package main

import (
	"darkzone/MultiTestServer/internal/db"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"
)

// Admin/debug protocol. Every handler here is gated on Client.isAdmin and is
// meant for exercising features by hand — it deliberately exposes internal AI
// state that the normal `state` broadcast does not carry.

// ServerDebugInfo is the payload of the S→C `debug_info` message.
type ServerDebugInfo struct {
	Players       int      `json:"players"`
	NPCs          int      `json:"npcs"`
	Gralats       int      `json:"gralats"`
	WorldItems    int      `json:"worldItems"`
	DefaultMap    string   `json:"defaultMap"`
	MyMap         string   `json:"myMap"`
	WorldW        float64  `json:"worldW"`
	WorldH        float64  `json:"worldH"`
	SpawnX        float64  `json:"spawnX"`
	SpawnY        float64  `json:"spawnY"`
	HasCollision  bool     `json:"hasCollision"`
	UptimeSec     int      `json:"uptimeSec"`
	ObservedHz    float64  `json:"observedHz"`
	LuaResources  []string `json:"luaResources"`
	ViewRadius    float64  `json:"viewRadius"`
	PlayersOnMyMap int     `json:"playersOnMyMap"`
}

// NPCDebugInfo is one row of the S→C `npc_debug_list` message.
type NPCDebugInfo struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	NPCType   int     `json:"npcType"`
	Behaviour string  `json:"behaviour"`
	AIState   string  `json:"aiState"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	HP        int     `json:"hp"`
	MaxHP     int     `json:"maxHp"`
	Alive     bool    `json:"alive"`
	MapID     string  `json:"mapId"`
	Aggro     string  `json:"aggro"`
	Blocked   float64 `json:"blocked"`
	Waypoints int     `json:"waypoints"`
	MountedBy string  `json:"mountedBy"`
	Dist      float64 `json:"dist"` // from the requesting player
}

// requireAdmin reports whether c may run admin commands, telling them if not.
func requireAdmin(c *Client) bool {
	if c.isAdmin {
		return true
	}
	sendDirectMsg(c, "Permission denied.")
	return false
}

// handleAdminDebugInfo reports server-wide state for the debug panel.
func handleAdminDebugInfo(c *Client) {
	if !requireAdmin(c) {
		return
	}
	myMap := mapOrDefault(c.currentMap)

	globalHub.mu.RLock()
	info := ServerDebugInfo{
		Players:      len(globalHub.clients),
		NPCs:         len(globalHub.npcs),
		Gralats:      len(globalHub.gralats),
		WorldItems:   len(globalHub.worldItems),
		DefaultMap:   defaultMap,
		MyMap:        myMap,
		WorldW:       globalHub.worldW,
		WorldH:       globalHub.worldH,
		SpawnX:       globalHub.spawnX,
		SpawnY:       globalHub.spawnY,
		HasCollision: globalHub.worldColl != nil,
		ViewRadius:   viewRadius,
	}
	for other := range globalHub.clients {
		if mapOrDefault(other.currentMap) == myMap {
			info.PlayersOnMyMap++
		}
	}
	ticks := globalHub.tickCount
	started := globalHub.startedAt
	globalHub.mu.RUnlock()

	if !started.IsZero() {
		up := time.Since(started).Seconds()
		info.UptimeSec = int(up)
		if up > 0 {
			info.ObservedHz = float64(ticks) / up
		}
	}
	if globalLuaManager != nil {
		info.LuaResources = globalLuaManager.List()
	}

	sendJSON(c, map[string]interface{}{"type": "debug_info", "info": info})
}

// handleAdminNPCList reports the NPCs on the caller's map, nearest first,
// including the AI internals the normal state broadcast omits.
func handleAdminNPCList(c *Client) {
	if !requireAdmin(c) {
		return
	}
	myMap := mapOrDefault(c.currentMap)
	cx, cy := c.state.X, c.state.Y

	globalHub.mu.RLock()
	rows := make([]NPCDebugInfo, 0, len(globalHub.npcs))
	for _, n := range globalHub.npcs {
		if mapOrDefault(n.mapID) != myMap {
			continue
		}
		rows = append(rows, NPCDebugInfo{
			ID:        n.state.ID,
			Name:      n.state.Name,
			NPCType:   n.state.NPCType,
			Behaviour: n.behaviour.String(),
			AIState:   n.state2.String(),
			X:         n.state.X,
			Y:         n.state.Y,
			HP:        n.combat.HP,
			MaxHP:     n.combat.MaxHP,
			Alive:     n.combat.IsAlive(),
			MapID:     mapOrDefault(n.mapID),
			Aggro:     n.aggroTarget,
			Blocked:   n.blockedTime,
			Waypoints: len(n.waypoints),
			MountedBy: n.mountedBy,
			Dist:      math.Hypot(n.state.X-cx, n.state.Y-cy),
		})
	}
	globalHub.mu.RUnlock()

	sort.Slice(rows, func(i, j int) bool { return rows[i].Dist < rows[j].Dist })
	// Key is deliberately not "npcs": that name already carries []NPCState in the
	// state broadcast, and the client decodes every message into one struct.
	sendJSON(c, map[string]interface{}{"type": "npc_debug_list", "debugNpcs": rows})
}

// handleAdminSetNPCBehaviour switches an NPC's movement behaviour at runtime.
func handleAdminSetNPCBehaviour(c *Client, raw []byte) {
	if !requireAdmin(c) {
		return
	}
	var msg struct {
		NPCID     string `json:"npc_id"`
		Behaviour string `json:"behaviour"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.NPCID == "" {
		return
	}
	b := ParseBehaviour(strings.ToLower(strings.TrimSpace(msg.Behaviour)))
	myMap := mapOrDefault(c.currentMap)

	globalHub.mu.Lock()
	n := globalHub.findNPCOnMap(msg.NPCID, myMap)
	if n != nil {
		n.SetBehaviour(b)
	}
	globalHub.mu.Unlock()

	if n == nil {
		sendDirectMsg(c, "NPC introuvable sur cette map: "+msg.NPCID)
		return
	}
	sendDirectMsg(c, fmt.Sprintf("[DEBUG] %s → comportement %q", msg.NPCID, b))
	handleAdminNPCList(c)
}

// handleAdminNPCAction applies a one-shot action to an NPC: kill, revive,
// provoke (simulate being attacked), or bring it to the caller.
func handleAdminNPCAction(c *Client, raw []byte) {
	if !requireAdmin(c) {
		return
	}
	var msg struct {
		NPCID  string `json:"npc_id"`
		Action string `json:"action"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.NPCID == "" {
		return
	}
	myMap := mapOrDefault(c.currentMap)
	px, py := c.state.X, c.state.Y

	globalHub.mu.Lock()
	n := globalHub.findNPCOnMap(msg.NPCID, myMap)
	if n != nil {
		switch msg.Action {
		case "kill":
			// Bypass the invulnerability window: this is a debug action.
			n.combat.hitCD = 0
			if n.combat.MaxHP == 0 {
				n.combat.alive = false // immortal entities can still be despawned here
			} else {
				n.combat.Damage(n.combat.HP)
			}
			n.syncState()
		case "revive":
			n.combat.alive = true
			n.combat.HP = n.combat.MaxHP
			n.combat.deathTimer = 0
			n.state2 = aiNormal
			n.aggroTarget = ""
			n.syncState()
		case "provoke":
			n.provoke()
		case "bring":
			n.state.X, n.state.Y = px, py
			n.homeX, n.homeY = px, py
			n.targetX, n.targetY = px, py
			n.SetWaypoints(nil)
			n.blockedTime = 0
		case "home":
			n.state.X, n.state.Y = n.homeX, n.homeY
			n.state2 = aiNormal
			n.blockedTime = 0
		}
	}
	globalHub.mu.Unlock()

	if n == nil {
		sendDirectMsg(c, "NPC introuvable sur cette map: "+msg.NPCID)
		return
	}
	sendDirectMsg(c, fmt.Sprintf("[DEBUG] %s → %s", msg.NPCID, msg.Action))
	handleAdminNPCList(c)
}

// handleAdminTeleport moves the caller. The client applies the authoritative
// position from the teleport_ok reply, since it owns movement locally.
func handleAdminTeleport(c *Client, raw []byte) {
	if !requireAdmin(c) {
		return
	}
	var msg struct {
		X   float64 `json:"x"`
		Y   float64 `json:"y"`
		Rel bool    `json:"rel"` // treat X/Y as an offset from the current position
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}

	globalHub.mu.Lock()
	tx, ty := msg.X, msg.Y
	if msg.Rel {
		tx += c.state.X
		ty += c.state.Y
	}
	c.state.X = clamp(tx, 0, globalHub.worldW-32)
	c.state.Y = clamp(ty, 0, globalHub.worldH-32)
	nx, ny := c.state.X, c.state.Y
	globalHub.mu.Unlock()

	sendJSON(c, map[string]interface{}{"type": "teleport_ok", "x": nx, "y": ny})
	log.Printf("[ADMIN] %s teleported to (%.0f,%.0f)", c.name, nx, ny)
}

// handleAdminSetGralats adds (or removes, with a negative amount) gralats.
func handleAdminSetGralats(c *Client, raw []byte) {
	if !requireAdmin(c) {
		return
	}
	var msg struct {
		Amount int `json:"amount"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.Amount == 0 {
		return
	}
	total, err := db.AddGralats(c.userID, msg.Amount)
	if err != nil {
		sendDirectMsg(c, "Erreur DB: "+err.Error())
		return
	}
	globalHub.mu.Lock()
	c.state.Gralats = total
	globalHub.mu.Unlock()

	sendJSON(c, map[string]interface{}{"type": "gralat_update", "gralat_n": total})
	sendDirectMsg(c, fmt.Sprintf("[DEBUG] gralats %+d → %d", msg.Amount, total))
}

// handleAdminSetHP forces the caller's HP, for testing damage and death states.
func handleAdminSetHP(c *Client, raw []byte) {
	if !requireAdmin(c) {
		return
	}
	var msg struct {
		HP int `json:"hp"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	hp := msg.HP
	if hp < 0 {
		hp = 0
	}
	if hp > playerMaxHP {
		hp = playerMaxHP
	}

	globalHub.mu.Lock()
	c.combat.HP = hp
	c.combat.alive = hp > 0
	c.combat.hitCD = 0
	c.state.HP = hp
	if hp == 0 {
		c.state.AnimState = "dead"
	} else if c.state.AnimState == "dead" {
		c.state.AnimState = ""
	}
	globalHub.mu.Unlock()

	sendJSON(c, map[string]interface{}{"type": "pvp_damage", "hp": hp, "killed": hp == 0})
	sendDirectMsg(c, fmt.Sprintf("[DEBUG] HP → %d", hp))
}

// handleAdminSpawnEnemyMsg is the menu equivalent of the /spawnenemy command.
func handleAdminSpawnEnemyMsg(c *Client, raw []byte) {
	if !requireAdmin(c) {
		return
	}
	var msg struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	name := strings.TrimSpace(msg.Name)
	if name == "" {
		name = "Ennemi"
	}
	n := msg.Count
	if n <= 0 {
		n = 1
	}
	if n > 20 {
		n = 20 // keep a stray click from flooding the world
	}
	mapID := mapOrDefault(c.currentMap)
	for i := 0; i < n; i++ {
		globalHub.spawnEnemyAt(name, mapID, c.state.X, c.state.Y)
	}
	sendDirectMsg(c, fmt.Sprintf("[DEBUG] %d × %q spawné sur %s", n, name, mapID))
}
