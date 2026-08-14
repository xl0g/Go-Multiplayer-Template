package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

const otherMap = "maps/tmx/cavern_01.tmx"

// newTestHub builds a Hub without touching the DB or the collision loader.
func newTestHub() *Hub {
	return &Hub{
		clients:     make(map[*Client]bool),
		gralatTimer: make(map[string]time.Time),
		gralatSpawn: make(map[string]GralatPickup),
		worldW:      1120,
		worldH:      1120,
	}
}

func newTestClient(id, mapID string, x, y float64) *Client {
	return &Client{
		send:         make(chan []byte, 8),
		playerID:     id,
		name:         id,
		currentMap:   mapID,
		combat:       newCombat(playerMaxHP),
		sessionStart: time.Now(),
		state: PlayerState{
			ID: id, Name: id, X: x, Y: y, HP: playerMaxHP, MaxHP: playerMaxHP,
		},
	}
}

// readState drains one "state" message from a client's send channel.
func readState(t *testing.T, c *Client) struct {
	Type       string           `json:"type"`
	Players    []PlayerState    `json:"players"`
	NPCs       []NPCState       `json:"npcs"`
	Gralats    []GralatPickup   `json:"gralats"`
	WorldItems []WorldSpawnItem `json:"world_items"`
} {
	t.Helper()
	var out struct {
		Type       string           `json:"type"`
		Players    []PlayerState    `json:"players"`
		NPCs       []NPCState       `json:"npcs"`
		Gralats    []GralatPickup   `json:"gralats"`
		WorldItems []WorldSpawnItem `json:"world_items"`
	}
	select {
	case data := <-c.send:
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("unmarshal state for %s: %v", c.playerID, err)
		}
	default:
		t.Fatalf("no state message queued for %s", c.playerID)
	}
	return out
}

// resolveDefaultMap must reproduce the map ID the client announces, otherwise
// every built-in NPC and gralat lands on an instance no player is on.
func TestResolveDefaultMap(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, spawnMap string) string {
		t.Helper()
		p := filepath.Join(dir, "config.json")
		body := `{"spawnMap": ` + strconv.Quote(spawnMap) + `}`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct {
		spawnMap string
		want     string
	}{
		// GMAP names are announced verbatim by the client.
		{"classiciphone.gmap", "classiciphone.gmap"},
		{"maps/gmap/classiciphone.gmap", "maps/gmap/classiciphone.gmap"},
		// A path already carrying a directory is used as-is.
		{"maps/GraalRebornMap.tmx", "maps/GraalRebornMap.tmx"},
		// Bare TMX names fall back to maps/ when maps/tmx/ has no such file.
		{"GraalRebornMap.tmx", "maps/GraalRebornMap.tmx"},
		// A suffixless bare name becomes .tmx.
		{"GraalRebornMap", "maps/GraalRebornMap.tmx"},
		// Empty config falls back to the compiled-in default.
		{"", "maps/GraalRebornMap.tmx"},
	}
	for _, tc := range cases {
		t.Run(tc.spawnMap, func(t *testing.T) {
			if got := resolveDefaultMap(write(t, tc.spawnMap)); got != tc.want {
				t.Errorf("resolveDefaultMap(spawnMap=%q) = %q, want %q", tc.spawnMap, got, tc.want)
			}
		})
	}

	// A missing config file must not panic and must yield the fallback.
	if got := resolveDefaultMap(filepath.Join(dir, "nope.json")); got != "maps/GraalRebornMap.tmx" {
		t.Errorf("missing config = %q, want the compiled-in default", got)
	}
}

// Built-in content is anchored to the configured spawn point; with no spawn
// configured it falls back to the world centre.
func TestResolveSpawnAndPlaceNear(t *testing.T) {
	h := newTestHub()
	h.worldW, h.worldH = 26624, 23552

	if x, y := h.resolveSpawn(serverConfig{SpawnX: 15918, SpawnY: 12003}); x != 15918 || y != 12003 {
		t.Errorf("configured spawn = (%.0f,%.0f), want (15918,12003)", x, y)
	}
	if x, y := h.resolveSpawn(serverConfig{}); x != 13312 || y != 11776 {
		t.Errorf("unset spawn = (%.0f,%.0f), want the world centre (13312,11776)", x, y)
	}

	// Offsets must land within viewRadius of spawn, or players never see them.
	h.spawnX, h.spawnY = 15918, 12003
	for _, def := range builtinNPCDefs {
		x, y := h.placeNear(def.dx, def.dy)
		dx, dy := x-h.spawnX, y-h.spawnY
		if dx*dx+dy*dy > viewRadius*viewRadius {
			t.Errorf("NPC %q placed %.0f px from spawn, beyond viewRadius %.0f",
				def.name, dx*dx+dy*dy, viewRadius)
		}
	}
	for _, d := range gralatSpawnDefs {
		x, y := h.placeNear(d.dx, d.dy)
		dx, dy := x-h.spawnX, y-h.spawnY
		if dx*dx+dy*dy > viewRadius*viewRadius {
			t.Errorf("gralat %q placed beyond viewRadius of spawn", d.id)
		}
	}

	// Offsets must stay inside the world even when spawn sits on an edge.
	h.spawnX, h.spawnY = 0, 0
	for _, def := range builtinNPCDefs {
		x, y := h.placeNear(def.dx, def.dy)
		if x < 0 || y < 0 || x > h.worldW-32 || y > h.worldH-32 {
			t.Errorf("NPC %q placed out of bounds at (%.0f,%.0f)", def.name, x, y)
		}
	}
}

func TestMapOrDefault(t *testing.T) {
	if got := mapOrDefault(""); got != defaultMap {
		t.Errorf("mapOrDefault(%q) = %q, want %q", "", got, defaultMap)
	}
	if got := mapOrDefault(otherMap); got != otherMap {
		t.Errorf("mapOrDefault(%q) = %q, want same", otherMap, got)
	}
}

// An immortal entity (MaxHP 0, e.g. a horse) must be alive, otherwise it is
// filtered out of state broadcasts, never ticked, and cannot be mounted.
func TestNewCombatImmortalIsAlive(t *testing.T) {
	c := newCombat(0)
	if !c.IsAlive() {
		t.Error("newCombat(0).IsAlive() = false, want true (immortal, not dead)")
	}
	if hp, killed := c.Damage(1); hp != -1 || killed {
		t.Errorf("Damage on immortal = (%d, %v), want (-1, false)", hp, killed)
	}
}

func TestFindNPCOnMapIsolatesInstances(t *testing.T) {
	h := newTestHub()
	onDefault := newNPC("npc_a", "Villager", 100, 100, NPCTypeVillager)
	onOther := newNPC("npc_b", "Guard", 100, 100, NPCTypeGuard)
	onOther.mapID = otherMap
	h.npcs = []*NPC{onDefault, onOther}

	if got := h.findNPCOnMap("npc_a", defaultMap); got != onDefault {
		t.Error("NPC with empty mapID should be found on the default map")
	}
	if got := h.findNPCOnMap("npc_b", defaultMap); got != nil {
		t.Error("NPC on another map must not be reachable from the default map")
	}
	if got := h.findNPCOnMap("npc_a", otherMap); got != nil {
		t.Error("default-map NPC must not be reachable from another map")
	}
	if got := h.findNPCOnMap("npc_b", otherMap); got != onOther {
		t.Error("NPC should be found on its own map")
	}
}

func TestDamageNPCRejectsCrossMap(t *testing.T) {
	h := newTestHub()
	n := newNPC("mob", "Slime", 100, 100, NPCTypeAggressive)
	n.mapID = otherMap
	h.npcs = []*NPC{n}

	if hp, killed := h.damageNPC("mob", defaultMap, 1); hp != -1 || killed {
		t.Errorf("cross-map damage = (%d, %v), want (-1, false)", hp, killed)
	}
	if n.combat.HP != n.combat.MaxHP {
		t.Errorf("cross-map hit changed HP: %d, want %d", n.combat.HP, n.combat.MaxHP)
	}
	hp, _ := h.damageNPC("mob", otherMap, 1)
	if hp != n.combat.MaxHP-1 {
		t.Errorf("same-map damage = %d, want %d", hp, n.combat.MaxHP-1)
	}
}

func TestMountNPCRespectsMapAndSucceedsOnHorse(t *testing.T) {
	h := newTestHub()
	horse := newNPC("horse_1", "Épona", 200, 200, NPCTypeHorse)
	h.npcs = []*NPC{horse}

	if h.mountNPC("horse_1", otherMap, "player_1") {
		t.Error("mounting a horse on another map must fail")
	}
	if !h.mountNPC("horse_1", defaultMap, "player_1") {
		t.Fatal("mounting a horse on the same map must succeed")
	}
	if horse.mountedBy != "player_1" || horse.state.MountedBy != "player_1" {
		t.Error("mount did not record the rider")
	}
	if h.mountNPC("horse_1", defaultMap, "player_2") {
		t.Error("a horse already ridden must not be mountable by a second player")
	}
}

func TestCollectGralatRequiresSameMapAndProximity(t *testing.T) {
	cases := []struct {
		name      string
		mapID     string
		px, py    float64
		wantValue int
	}{
		{"same map, in reach", defaultMap, 105, 105, 5},
		{"same map, out of reach", defaultMap, 900, 900, 0},
		{"other map, same coords", otherMap, 105, 105, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHub()
			h.gralats = []*GralatPickup{{ID: "g1", X: 100, Y: 100, Value: 5, MapID: defaultMap}}

			got := h.collectGralat("g1", tc.mapID, tc.px, tc.py)
			if got != tc.wantValue {
				t.Errorf("collectGralat = %d, want %d", got, tc.wantValue)
			}
			wantRemaining := 1
			if tc.wantValue > 0 {
				wantRemaining = 0
			}
			if len(h.gralats) != wantRemaining {
				t.Errorf("gralats remaining = %d, want %d", len(h.gralats), wantRemaining)
			}
		})
	}
}

// A collected gralat must reappear at the wall-free position resolved at
// startup, not at its raw definition coordinates.
func TestCheckRespawnsUsesResolvedPosition(t *testing.T) {
	h := newTestHub()
	resolved := GralatPickup{ID: "g1", X: 264, Y: 312, Value: 30, MapID: defaultMap}
	h.gralatSpawn["g1"] = resolved
	h.gralatTimer["g1"] = time.Now().Add(-time.Second) // already due

	h.checkRespawns()

	if len(h.gralats) != 1 {
		t.Fatalf("gralats after respawn = %d, want 1", len(h.gralats))
	}
	got := h.gralats[0]
	if got.X != resolved.X || got.Y != resolved.Y {
		t.Errorf("respawned at (%.0f,%.0f), want (%.0f,%.0f)", got.X, got.Y, resolved.X, resolved.Y)
	}
	if got.MapID != defaultMap {
		t.Errorf("respawned MapID = %q, want %q", got.MapID, defaultMap)
	}
	if len(h.gralatTimer) != 0 {
		t.Errorf("respawn timer not cleared: %d entries left", len(h.gralatTimer))
	}
	// The stored definition must not be aliased by the live pickup.
	got.X = -1
	if h.gralatSpawn["g1"].X != resolved.X {
		t.Error("respawn aliased the stored spawn definition")
	}
}

// sendPerClientState must scope players, NPCs, gralats and world items to the
// receiver's map instance. Both players sit at the same coordinates on purpose:
// coordinate spaces repeat across maps, so only the map ID separates them.
func TestSendPerClientStateIsolatesMaps(t *testing.T) {
	h := newTestHub()

	alice := newTestClient("player_alice", defaultMap, 300, 300)
	bob := newTestClient("player_bob", otherMap, 300, 300)
	h.clients[alice] = true
	h.clients[bob] = true

	npcDefault := newNPC("npc_default", "Villager", 320, 320, NPCTypeVillager)
	npcOther := newNPC("npc_other", "Guard", 320, 320, NPCTypeGuard)
	npcOther.mapID = otherMap
	h.npcs = []*NPC{npcDefault, npcOther}

	h.gralats = []*GralatPickup{
		{ID: "g_default", X: 310, Y: 310, Value: 1, MapID: defaultMap},
		{ID: "g_other", X: 310, Y: 310, Value: 1, MapID: otherMap},
	}
	h.worldItems = []*WorldSpawnItem{
		{ID: "wi_default", Name: "Pot", X: 305, Y: 305, MapID: defaultMap},
		{ID: "wi_other", Name: "Chest", X: 305, Y: 305, MapID: otherMap},
	}

	h.sendPerClientState()

	for _, tc := range []struct {
		c                                  *Client
		wantPlayer, wantNPC, wantG, wantWI string
	}{
		{alice, "player_alice", "npc_default", "g_default", "wi_default"},
		{bob, "player_bob", "npc_other", "g_other", "wi_other"},
	} {
		got := readState(t, tc.c)
		if got.Type != "state" {
			t.Errorf("%s: type = %q, want \"state\"", tc.c.playerID, got.Type)
		}
		if len(got.Players) != 1 || got.Players[0].ID != tc.wantPlayer {
			t.Errorf("%s: players = %+v, want only %s", tc.c.playerID, got.Players, tc.wantPlayer)
		}
		if len(got.NPCs) != 1 || got.NPCs[0].ID != tc.wantNPC {
			t.Errorf("%s: npcs = %+v, want only %s", tc.c.playerID, got.NPCs, tc.wantNPC)
		}
		if len(got.Gralats) != 1 || got.Gralats[0].ID != tc.wantG {
			t.Errorf("%s: gralats = %+v, want only %s", tc.c.playerID, got.Gralats, tc.wantG)
		}
		if len(got.WorldItems) != 1 || got.WorldItems[0].ID != tc.wantWI {
			t.Errorf("%s: world items = %+v, want only %s", tc.c.playerID, got.WorldItems, tc.wantWI)
		}
	}
}

// A horse must reach clients: it is immortal (MaxHP 0) and used to be filtered
// out of every broadcast because it was born dead.
func TestHorseIsBroadcast(t *testing.T) {
	h := newTestHub()
	rider := newTestClient("player_1", defaultMap, 200, 200)
	h.clients[rider] = true
	h.npcs = []*NPC{newNPC("horse_1", "Épona", 210, 210, NPCTypeHorse)}

	h.sendPerClientState()

	got := readState(t, rider)
	if len(got.NPCs) != 1 || got.NPCs[0].ID != "horse_1" {
		t.Fatalf("npcs = %+v, want the horse to be broadcast", got.NPCs)
	}
	if got.NPCs[0].AnimState == "dead" {
		t.Error("horse broadcast with anim \"dead\"")
	}
}
