# Graal Reborn

A real-time multiplayer game written in Go with Ebiten inspired by Graal Online Classic. Runs natively on Linux and in the browser (WebAssembly).

---

## Demonstration 
[Showcase](https://www.youtube.com/watch?v=VySwpsBMfQg)
## Features

- Account registration and login with persistent storage (SQLite)
- Real-time multiplayer - positions synchronized at 60 Hz
- Client-side interpolation - smooth movement without teleportation
- In-game chat broadcast to all connected players
- NPCs system (villagers, merchants, guards, horses) with wandering AI
- Sword combat system - hit NPCs, collect gralats
- Gani animation system (idle, walk, sword, ride, sit, push, dead, grab, dance..)
- TMX tile map with collision and interactive signs
- Gralat currency - collect coins, earn rewards from NPCs
- Full player profile panel with character preview and playtime tracking
- Cosmetic picker - body, head, and hat customization
- Linux native and browser (WASM) support
- PoC for shops

---

## Quick Start

### 1. Build

```bash
./build.sh
```

Produces `game-server` and `game-client`.

### 2. Start the server

```bash
./game-server
```

The database `game.db` is created automatically.

### 3. Run the client

```bash
./game-client

# Remote server:
./game-client -server myserver.com:8080
```
or the web version : 

Open **http://localhost:8080** in your browser.

---

## Controls

| Key | Action |
|-----|--------|
| `WASD` / Arrows | Move |
| `X` | Sword swing |
| `Q` | Grab |
| `TAB` | Open Admin / Debug panel (admin accounts only) |
| `R` | Mount / dismount horse |
| `F` | Interact with sign or NPC / close dialog |
| `T` | Open chat |
| `C` | Open cosmetic picker |
| `P` | Toggle own profile panel |
| `Click` player | Open that player's profile |
| `Enter` | Send message / confirm form |
| `Tab` | Switch field in menus |
| `Esc` | Close overlay / back to menu |

---

## Player Profile

Clicking on another player opens a full profile panel showing:

- **Name** - their account username
- **Fortune** - total gralats accumulated
- **Playtime** - total time played (live, updated each second)
- **Status** - Online badge
- **Character preview** - their cosmetic appearance rendered at 2.5× scale on the right side of the panel

Press **P** to open your own profile with the same information.
---

# To-do 

- [x] Shops POC
- [x] AI NPCs
- [x] Improving Sync
- [x] Add Lua scripting & expose bindings (cf [Resources documentation](resources/README.md))
- [x] Guild / Friends
- [ ] Improve Instance system
- [ ] Improve Inventory
- [ ] Interactions with world (WIP)
- [ ] Improve AI overall and add behavior (agressive, passive, roaming..)
- [ ] Mounts
- [ ] Quests 
- [ ] Body color
- [ ] Rework the UI
- [ ] Rework the backend to add TLS/WSS and rate limit, support loadbalancing
- [ ] Add Map Chunk loading through CDN
- [ ] Support for IOS, Android & Windows

For more information please check [ARCHITECTURE.MD](docs/ARCHITECTURE.MD)
