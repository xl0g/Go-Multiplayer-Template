# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build Commands

```bash
# Build server + native Linux client
./build.sh
# Outputs: ./game-server, ./game-client

# Build WASM client (placed in server/static/)
./build_web.sh

# Run server — requires a reachable PostgreSQL; schema is migrated on startup
./game-server
PORT=9090 DATABASE_URL='postgres://user:pass@localhost:5432/graalreborn?sslmode=disable' ./game-server

# Grant admin rights to an existing account, then exit
./game-server -setadmin <username>

# Run native client
./game-client
./game-client -server myserver.com:8080

# Server tests (no database needed — they build a Hub directly)
cd server && go test -race ./...
```

The server must be run from the repository root: it resolves `config.json`,
`maps/`, `assets/` and `server/static/` as relative paths.

The client and server are **separate Go modules** (`go.mod` at root for client, `server/go.mod` for server). Run `go mod tidy` in each directory independently.

## Architecture

This is a real-time multiplayer game with two separate Go programs sharing a JSON-over-WebSocket protocol.

### Client (`client/`, package `main` in module `darkzone/MultiTest`)

All client filenames below are relative to `client/`; the module root only holds
`go.mod`, `config.json` and the build scripts.

Built with [Ebiten](https://ebitengine.org/). Implements `ebiten.Game` in `game.go` as a **state machine** with states `StateMainMenu`, `StateLogin`, `StateRegister`, `StatePlaying`.

Key flow:
1. `main.go` → loads assets, creates `Game`, hands off to Ebiten's loop (60 TPS)
2. `Game.Update()` dispatches to the active state; in `StatePlaying`, calls `updatePlaying()` which drains the network channel then processes input
3. After login/register, `startGame()` spawns a goroutine that dials the WebSocket; auth token is sent as the first message
4. Network messages arrive via `Connection.TryReceive()` (non-blocking channel poll) and are processed in `handleServerMsg()`

**Platform split:** `network_native.go` (`!js` build tag) uses gorilla/websocket with goroutines; `network_js.go` (WASM) uses `syscall/js` browser WebSocket API. Both produce the same `*Connection` interface defined in `network.go`.

**Client-side interpolation:** Remote entities (`Character.TargetX/Y`) are set from server state; display position (`Character.X/Y`) exponentially decays toward target each frame (`interpK = 20.0`). The local player skips interpolation and moves immediately.

**Cosmetics:** Body/head/hat images are loaded asynchronously in goroutines when filenames change (`SetCosmetics` → sets `cosDirty` → goroutine loads from `Assets/offline/levels/`). Cosmetic state is protected by `Character.cosmu` mutex.

Sprite sheet layout: body frames are 32×32 px, arranged as `[col=direction][row=animFrame]`; head frames are 32×32 px per direction; hat frames are 48×48 px per direction.

**Camera zoom:** Mouse wheel zooms the viewport in/out (clamped to `Cfg.ZoomMin`–`Cfg.ZoomMax`). The world is rendered into an offscreen `worldBuf` then scaled; UI is always drawn at native resolution on top.

### Server (`server/` package `darkzone/MultiTestServer`)

Split across several files (`server/server.go` is an empty placeholder that only
documents the split):

| File | Responsibility |
|------|----------------|
| `main.go` | entry point, route table, `-setadmin` |
| `hub.go` | `Hub` — client registry, 60 Hz game loop, per-client state broadcast |
| `client.go` | WebSocket lifecycle and every `C → S` message handler |
| `npc.go` | `NPC`, AI (wander / aggressive / passive), dialog defs, built-in NPC table |
| `entity.go` | `CombatEntity` — HP, damage, invulnerability, respawn (shared by NPCs *and* players) |
| `spatial.go` | `tempGrid` interest-management grid, `viewRadius` |
| `gralat.go` | gralat spawn offsets, `findFreePos` |
| `items.go` / `handlers.go` | item defs; HTTP register/login/asset listing |
| `map_handler.go`, `tmx_parser.go`, `nw_format.go`, `world_collision.go` | map/chunk API and collision loading |
| `lua_manager.go`, `lua_bindings.go` | Lua resource system (`resources/*/`) |
| `internal/db/` | PostgreSQL layer (`pgx`) |

- **PostgreSQL** (via `github.com/jackc/pgx/v5`) — `DATABASE_URL`, default
  `postgres://postgres:postgres@localhost:5432/graalreborn`. `db.migrate()` runs
  `CREATE TABLE IF NOT EXISTS` + idempotent `ALTER TABLE ... ADD COLUMN IF NOT
  EXISTS` on every startup, so adding a column means adding a line there.
  Tables: `users`, `sessions`, `chat_history`, `inventory`, `world_items`,
  `friends`, `guilds`, `quests`.
- **HTTP REST** — `POST /api/register`, `POST /api/login` return a session token
- **WebSocket hub** — `Hub` manages connected `Client`s under a single `sync.RWMutex`
  that guards `clients`, every `Client.state`/`currentMap`, and every `NPC`
- **Game loop** — `Hub.runGameLoop()` ticks at 60 Hz: NPC AI, NPC attacks, Lua
  timers, gralat respawns, then `sendPerClientState()`
- **NPC AI** — 15 built-in NPCs (`builtinNPCDefs`) plus Lua- and admin-spawned
  ones. See the behaviour section below.
- **Map chunk API** — serves `.gmap` metadata and NW chunk data (layers, NPCs, signs, warp links) for the GMAP chunk streaming system

**Interest management:** `sendPerClientState()` sends each client its own
filtered snapshot rather than broadcasting the whole world — same map instance,
then within `viewRadius` (2048 px) using the per-tick spatial grid.

### NPC behaviours (`npc.go`)

`NPCType` decides how an NPC looks and what it says; **`Behaviour` decides how it
moves**. They are separate on purpose — a merchant can patrol and a guard can
stand still without inventing a type for each combination. `defaultBehaviourFor`
supplies the historical type→behaviour mapping when none is given.

| Behaviour | Movement |
|-----------|----------|
| `BehaviourWander` | short random walk around home (default) |
| `BehaviourRoam` | wide random walk (`roamRadius`), rarely idle |
| `BehaviourPatrol` | walks a waypoint loop, pausing at each stop |
| `BehaviourPassive` | flees the nearest player |
| `BehaviourAggressive` | chases and attacks |
| `BehaviourStatic` | never moves |

On top of the behaviour sits a small state machine (`aiState`): `aiNormal` →
`aiChasing` → `aiReturning` → `aiNormal`. Anything can be pulled into a chase;
when the chase ends the NPC walks home rather than settling wherever it stopped.

- **Leash** (`aggroLeashRange`, or `NPC.leashRange`) caps how far an NPC may be
  dragged from home before it gives up. Without it a player can pull a monster
  across the world and abandon it there.
- **Alerting**: entering a chase, or taking a hit (`NPC.provoke`), raises an
  alert. `Hub.propagateAlerts` pulls in allies within `alertRadius` on the same
  map; passive NPCs never answer. `alertCooldown` stops a long fight from
  re-alerting every tick. `raisedAlert` is set either by the AI tick or by
  `damageNPC` between ticks, and `propagateAlerts` is the only place that clears
  it — do not clear it in `NPC.update`.

**Movement invariant — `moveToward` and progress.** All movement goes through
`NPC.moveToward`, which counts progress as *getting closer to the destination
than the NPC has ever been* (`bestDist`), not as "a collision test passed" and
not as "the position changed". Both weaker definitions silently freeze NPCs:

- with an axis-aligned destination (`dy == 0`, which every `squarePatrol` leg is)
  the candidate Y equals the current Y, so the Y collision test passes on the
  position the NPC already occupies;
- while unsticking, the sideways detour changes the position without getting any
  nearer, and oscillating along a wall repeatedly re-approaches the target.

Either one resets the stall timers every tick, so no behaviour ever gives up on
an unreachable destination and the NPC stands still forever while reporting
`Moving = true`. `blockedTime` accumulates only while there is no new best, and
each behaviour uses it to re-target (`blockedRetarget*`). Spawn placement has the
matching rule: use `Hub.freeBoxNear`, which validates the **whole 28×28 box**
(`npcW`/`npcH`), not a single point — a point-validated spawn can leave an NPC
embedded in a wall.

### Map instances (read this before touching any interaction handler)

Every player and NPC belongs to a **map instance**, identified by a map name:
`Client.currentMap` (set by the `change_map` message) and `NPC.mapID`. Entities
on different instances must never see or affect each other.

Rules that are easy to break:

1. **Never compare map IDs directly.** An empty string means "the default map",
   so always normalize through `mapOrDefault()`. Comparing `""` with
   `defaultMap` silently creates two instances out of one.
2. **`defaultMap` is a `var`, not a constant.** It is derived from
   `config.json` at startup by `resolveDefaultMap()`, which mirrors the client's
   own map naming (`Game.activeGMap` in GMAP mode, `Game.currentMapName` in TMX
   mode). If it stops matching what the client announces, every entity with an
   empty `mapID` lands on an instance no player is ever on: the world goes
   silently empty with no error anywhere.
3. **Coordinate spaces repeat across maps**, so a proximity check alone proves
   nothing — a player on one map is "next to" an entity at the same coordinates
   on every other map. Any handler resolving a target by ID must scope it to the
   actor's map: use `Hub.findNPCOnMap(id, mapID)` for NPCs, and compare
   `mapOrDefault(other.currentMap)` for players. This applies to `talk_npc`,
   `sword_hit`, `pvp_hit`, `mount_npc`, `collect_gralat` and `buy_world_item`.
4. **Client-supplied coordinates are never trusted alone.** `collect_gralat` and
   the combat handlers validate distance server-side (`gralatReach`, `maxReach`).
5. **Built-in content is positioned relative to the spawn point**
   (`builtinNPCDefs`, `gralatSpawnDefs` hold `dx/dy` offsets resolved by
   `Hub.placeNear`). Never reintroduce absolute world coordinates: they only ever
   make sense for one map size.

`server/instance_test.go` covers these rules and runs without a database.

**Immortality convention:** `CombatEntity.MaxHP == 0` means *immortal* (horses),
not *dead* — `Damage()` returns `-1` for such entities and `newCombat` keeps them
`alive`. An immortal entity that is marked dead disappears from every broadcast,
is never ticked, and cannot be interacted with.

WebSocket lifecycle: first message must be `{"type":"auth","token":"..."}` within 10 s, then the client is registered in the hub. A writer goroutine drains `client.send` and sends WebSocket pings every 54 s. Read deadline resets on each message or pong.

The server also serves `server/static/` as the web root (WASM client after `./build_web.sh`).

### WebSocket Protocol (JSON)

| Direction | `type` | Key fields |
|-----------|--------|------------|
| C → S | `auth` | `token` |
| C → S | `move` | `x`, `y`, `dir`, `moving`, `anim`, `mounted` |
| C → S | `change_map` | `map` — announces the client's map instance |
| C → S | `chat` | `msg` (also carries `/`-commands, see below) |
| C → S | `cosmetic` | `body`, `head`, `hat`, `shield`, `sword` (filenames) |
| C → S | `collect_gralat` | `gralat_id` |
| C → S | `talk_npc` | `npc_id` |
| C → S | `mount_npc` | `npc_id` |
| C → S | `dismount` | — |
| C → S | `anim_state` | `anim`, `mounted` (also drives client-side respawn) |
| C → S | `sword_hit` | `npc_id` |
| C → S | `pvp_hit` | `target_id` |
| C → S | `use_item` | `item_id` |
| C → S | `buy_world_item` | `id` |
| C → S | `admin_spawn_world_item` | `name`, `sprite`, `item_id`, `price`, `x`, `y` |
| C → S | `admin_remove_world_item` | `id` |
| C → S | `friend_add` / `friend_accept` / `friend_remove` | `target` / `from` / `target` |
| C → S | `friend_list` / `guild_info` / `guild_list` / `quest_list` | — |
| C → S | `guild_create` | `name`, `tag`, `description` |
| C → S | `guild_join` / `guild_leave` | `guild_id` / — |
| C → S | `quest_start` | `quest_id` |
| S → C | `auth_ok` | `id`, `name`, `x`, `y`, `map`, `gralat_n`, `playtime`, cosmetics, `is_admin` |
| S → C | `auth_error` | `msg` |
| S → C | `state` | `players[]`, `npcs[]`, `gralats[]`, `worldItems[]` (60 Hz) |
| S → C | `chat` | `from`, `msg` |
| S → C | `system` | `msg` |
| S → C | `gralat_update` | `gralat_n` (the player's new total) |
| S → C | `npc_dialog` | `msg`, `gralat_n` |
| S → C | `npc_damage` | `npc_id`, `hp`, `killed` |
| S → C | `pvp_damage` | `from`, `damage`, `atk_x`, `atk_y`, `hp`, `killed` |
| S → C | `mount_ok` / `dismount_ok` | `npc_id` / — |
| S → C | `buy_result` | `success`, `msg`, `gralat_n`, `item_id` |
| S → C | `friend_list` | `friends[]`, `requests[]` |
| S → C | `friend_request` | `from` |
| S → C | `friend_result` | `msg` |
| S → C | `guild_info` | `guild` |
| S → C | `guild_list` | `guilds[]` |
| S → C | `guild_result` | `ok`, `msg` |
| S → C | `quest_list` | `quests[]` |
| S → C | `quest_update` | `quest_id`, `progress` |
| S → C | `inventory` | `items[]` |

### Chat Commands (server-side, handled in `handleChat`)

Messages starting with `/` are intercepted and never broadcast as chat:

| Command | Access | Effect |
|---------|--------|--------|
| `/itemlist` | all | lists known item IDs |
| `/giveitem <player> <item_id>` / `/removeitem <player> <item_id>` | admin | inventory edits, refreshed live if the target is online |
| `/spawnenemy [name]` | admin | spawns an aggressive NPC on the caller's map |
| `/resources` | admin | lists running Lua resources |
| `/start` / `/stop` / `/restart <resource>` | admin | Lua resource control |

Admin rights come from `users.is_admin`, set with `./game-server -setadmin <user>`.

### Lua Resources (`resources/<name>/`)

`LuaManager` loads each resource directory and runs its scripts in isolated
states. Script selection order: `__resource.lua` manifest (`server_scripts`) →
`server.lua` → all `*.lua` sorted. `LuaManager.Tick()` is driven by the game loop
and runs timers plus queued events.

Bindings (`lua_bindings.go`):

- NPCs — `CreateNPC`, `DeleteNPC`, `SetNPCPosition`, `GetNPCPosition`,
  `SetNPCDialog`, `SetNPCBehaviour(id, "wander"|"roam"|"patrol"|"passive"|"aggressive"|"static")`,
  `SetNPCWaypoints(id, {{x=..,y=..}, ...})`
- Players — `GetPlayers`, `GetPlayerName`, `GetPlayerPos`, `GiveGralats`, `TakeGralats`
- Messaging — `SendMessage`, `BroadcastMessage`, `BroadcastChat`
- Timers — `SetTimeout`, `SetInterval`, `ClearTimer`
- Events — `AddEventHandler`, `TriggerEvent`; the server fires
  `onPlayerConnect`, `onPlayerDisconnect`, `onPlayerChat`
- Misc — `RandomInt`, `GetResourceName`

NPCs created from Lua adopt the hub's world bounds and land on the **default map
instance** unless their `mapID` is set.

### Config System (`config.go` / `config.json`)

`config.json` in the repo root is loaded at startup into the global `Cfg` (`GameConfig`). Missing keys fall back to compiled-in defaults.

| Field | Default | Description |
|-------|---------|-------------|
| `serverURL` | `localhost:8080` | Server address |
| `chunkRadius` | `2` | Minimum chunk load radius around player |
| `playerSpeed` | `700` | Movement speed in px/s |
| `mountedSpeed` | `320` | Speed while mounted |
| `zoomMin` / `zoomMax` | `0.35` / `2.5` | Camera zoom limits |
| `spawnMap` | `maps/GraalRebornMap.tmx` | Map file loaded on startup (`.tmx` or `.gmap`) |
| `spawnX` / `spawnY` | `0` / `0` | World-pixel spawn position |

### GMAP Chunk-Based World (`gmap.go`)

When `Cfg.SpawnMap` ends in `.gmap`, the client enters **chunk-streaming mode** via `ChunkManager`:
- The server is queried for GMAP metadata (`/api/gmap/<name>`) which provides a grid of NW chunk filenames.
- Chunks within `viewRadius` of the player are fetched asynchronously (`/api/chunk/<name>`); chunks outside are evicted.
- `viewRadius` is the larger of `Cfg.ChunkRadius` and half the screen coverage at the current zoom level.
- Each chunk is **64×64 tiles × 16 px** = 1024×1024 px. Tiles use `classiciphone_pics4.png` (128-col tileset, 16×16 px tiles, 1-based GID).
- Chunk layers carry `collision`, `terrain` (`"water"` / `"lava"`), NPCs, signs, and warp links.
- `ChunkManager.WarpAt()` returns the first warp link overlapping a world-space AABB; `game_input.go` triggers a map transition on overlap (with 2 s cooldown).
- Map transitions: `switchMap()` replaces the active `ChunkManager`; the previous one is kept as `prevChunkMgr` for instant rollback if the switch fails.

### TMX Map System (`tilemap.go`)

When `Cfg.SpawnMap` ends in `.tmx`, a single-file map is parsed. Tile IDs reference `classiciphone_pics4.png` (64-col 32×32 tileset, 1-based GID). Layer properties drive behavior:
- `collision=true` → non-zero tiles become solid (`GameMap.IsBlocked()`)
- `panneau="<text>"` → non-zero tiles become readable signs (F key when adjacent)
- `terrain=water` / `terrain=lava` → swim/lava animation triggers

`handleMovement()` in `game.go` splits X/Y movement and calls `IsBlocked()` independently for wall-sliding.

### Gani Animation System (`gani.go`)

Full parser and player for the `.gani` format (Graal Online animations). Key details:
- Base framerate 20 fps (0.05 s/tick); `WAIT N` holds a frame for `(1+N)*0.05 s`.
- ANI section: 4 consecutive non-blank lines = 1 frame (up / left / down / right).
- Each dir-line: comma-separated `spriteIdx dx dy` tokens.
- `ATTACHSPRITE` / `ATTACHSPRITE2` (draw-under) for layered sprites.
- Gani origin is top-left of 48×48 bounding box; body sprite at offset `(8,16)`.
- Active gani is selected per frame based on player state: `idle`, `walk`, `swim`, `lava`, `sword`, `grab`, `sit`, `mount`…
- Sources: `sprites.png` (shadow), `body.png`, `head.png`, `hat.png`, `sword1.png`, `ride.png`, `shield1.png`.

### Terrain System

`Game.terrainAt(x, y, w, h)` queries the active map (GMAP or TMX) for the terrain type at a world-space rect. Returns `"water"`, `"lava"`, or `""`. Called each frame for the local player's hitbox to switch swim/lava ganis and adjust movement behaviour.

### Mount System

- `R` key: mount the nearest rideable NPC → sends `mount_npc`; dismounts with another `R` → sends `dismount`.
- While mounted, `Character.Mounted = true`; speed uses `Cfg.MountedSpeed`; the `ride` gani plays.
- Server broadcasts mount state in `PlayerState`, and mirrors the rider's position onto the horse via `Hub.updateHorsePos` on every `move`.
- Only `NPCTypeHorse` NPCs are rideable, and only one rider at a time
  (`NPC.mountedBy`). `Hub.unregister` frees the horse when a rider disconnects.
- Horses are immortal (`MaxHP == 0`) — see the immortality convention above.

### Sword & Grab

- `X` key: triggers sword swing → plays sword gani; when the hit frame activates, sends `sword_hit` with target id.
- `Q` key (AZERTY physical A) / on-screen glove button: grab — held continuously; releases on key-up.
- Both actions block movement while active.

### Gralat Currency System

`GralatPickup` is defined in `types.go` on both sides. The server owns the authoritative list of world pickups, sending the ones on the player's own map inside every `state` message. The client auto-collects on overlap and sends `collect_gralat`; the server validates **map instance and distance** (`gralatReach`) before removing it, crediting the DB and replying `gralat_update`. NPCs give gralats via `talk_npc` → `npc_dialog` (120 s cooldown per NPC per player). Gralats persist in the `users.gralats` DB column. Each player's count is included in `PlayerState` and displayed in the HUD (top-centre) and the profile panel (`P` key). Other players can see the count in name tags.

Sprite sheet `Assets/offline/levels/images/downloads/gralats.png` is 64×64 (2×2 grid of 32×32): top-left=1, top-right=5, bottom-left=30, bottom-right=100. Loading and sprite selection is in `gralat.go`.

### Inventory System (`inventory.go`)

`InventoryMenu` shows the player's usable items in a 5-column grid. Items are synced from the server via `inventory` messages. The player selects a slot and presses Enter / clicks to use it; `InventoryMenu.UsedItem` is consumed by `game.go` which sends the appropriate action. Open with `I`.

### Social Panel (`social_panel.go` + `panel_menu.go`)

`SocialPanel` is a unified bottom panel with three sub-tabs:
- **Friends** — list, pending requests, add-friend text box.
- **Guilds** — own guild info, member list, global guild search, create/join/leave.
- **Quests** — scrollable quest log with progress bars.

`PanelMenu` is the icon strip at the top of screen (Maps, News, Shop, Friends, Guilds, Quests, Settings…). Clicking an icon opens the corresponding sub-panel or action.

### Admin Menu (`admin.go`)

Accessible via `Tab` key for admin accounts only (`g.isAdmin`). Lets admins:
- Spawn world items (name, sprite path, item id, price) at the local player's position.
- Remove existing world items by id (shown in a list synced from server).

Signals `SpawnReq` / `RemoveID` are set on the struct and consumed by `game.go` each frame.

### Emoji / Emoticons (`emoji.go`)

When a player sends chat text matching a shortcode (`:)`, `:(`, etc.) an emoticon bubble is displayed above their head for `emojiBubbleDuration` seconds. Images are loaded from `assets/offline/levels/emoticons/`.

### AnimImage (`animimage.go`)

`AnimImage` wraps either a static PNG or an animated GIF. Raw frames are decoded in a background goroutine; Ebiten textures are lazily created on the main thread on first access. Used for cosmetic icons and panel images.

### F3 Debug Overlay

`F3` toggles `g.debugOverlay`. Draws:
- Loaded chunk borders (green) and loading chunks (red) scaled to screen-space.
- Chunk grid labels (`col,row`).
- NW warp link zones (orange).
- Player world-pixel coordinates and current chunk.
- Minimap placeholder region.

### Controls (in-game)

| Key | Action |
|-----|--------|
| `ZQSD` / Arrows | Move (with tile collision) |
| `X` | Sword swing |
| `Q` / on-screen glove | Grab (hold) |
| `R` | Mount / dismount nearest NPC |
| `F` | Read adjacent sign / talk to nearby NPC / close dialog |
| `I` | Toggle inventory |
| `P` | Toggle profile panel (gralat count) |
| `T` | Open chat |
| `C` | Open cosmetic picker |
| `Tab` | Toggle admin menu (admin only) |
| `F3` | Toggle debug overlay |
| Mouse wheel | Camera zoom in/out |
| `Esc` | Close dialog / close panel / back to menu |

### Key Files

- `types.go` — `PlayerState`, `NPCState`, `ServerMessage` (client-side deserialization)
- `character.go` — `Character` (both local and remote entities, rendering + interpolation)
- `config.go` — `GameConfig`, `LoadConfig`, global `Cfg`
- `gmap.go` — `ChunkManager`, `Chunk`, GMAP streaming
- `tilemap.go` — TMX parser, `GameMap`
- `gani.go` — `.gani` format parser and frame player
- `animimage.go` — static PNG / animated GIF wrapper
- `emoji.go` — emoticon bubble system
- `inventory.go` — `InventoryMenu`
- `admin.go` — `AdminMenu`
- `social_panel.go` — `SocialPanel` (friends / guilds / quests)
- `panel_menu.go` — `PanelMenu` top icon strip
- `ui.go` — primitive drawing helpers (`DrawRect`, `DrawText`) and reusable widgets
- `menu.go` — login/register menus; perform HTTP calls to `/api/login` and `/api/register`
- `chat.go` — in-game chat overlay
- `cosmetic.go` — cosmetic picker menu (`CosmeticMenu`)
- `gralat.go` — gralat pickup rendering and sprite selection
- `game.go` — `Game` struct, state machine, map switching
- `game_draw.go` — all rendering logic
- `game_input.go` — keyboard/mouse input, movement, interactions
- `game_network.go` — `handleServerMsg()`, all server message handlers

### Asset Paths

- `assets/character.png`, `assets/head.png`, `assets/tiles.png` — default sprites (missing files are tolerated)
- `Assets/offline/levels/bodies/`, `heads/`, `hats/` — cosmetic sprites loaded at runtime
- `GANITEMPLATE/res/ganis/` — gani animation files
- `GANITEMPLATE/res/images/` — sprite images used by gani (`sprites.png`, `sword1.png`, `ride.png`, `shield1.png`)
- `assets/offline/levels/emoticons/` — emoticon images
- `maps/tmx/` — collection of TMX map files
- `config.json` — runtime configuration (repo root)
