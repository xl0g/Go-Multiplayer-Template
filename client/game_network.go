package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// remoteVel returns the dead-reckoning velocity (px/s) for a remote entity
// based on its current direction and moving flag.
// This is used to predict where the entity will be between server snapshots,
// compensating for the one-way network latency.
func remoteVel(dir int, moving bool, speed float64) (vx, vy float64) {
	if !moving {
		return 0, 0
	}
	switch dir {
	case 0: // up
		return 0, -speed
	case 1: // left
		return -speed, 0
	case 2: // down
		return 0, speed
	case 3: // right
		return speed, 0
	}
	return 0, 0
}

// npcDeadReckonSpeed returns the dead-reckoning speed for an NPC type.
// Chasing NPCs (aggressive/spawned) move diagonally toward the player, so
// cardinal-direction dead-reckoning would overshoot — they use 0 (pure
// interpolation toward the last known position instead).
func npcDeadReckonSpeed(npcType int) float64 {
	switch npcType {
	case NPCTypeSpawnedEnemy, NPCTypeAggressive:
		return 0 // pure interpolation: no cardinal-direction prediction
	case NPCTypePassive:
		return 120.0
	default:
		return 95.0
	}
}

// ──────────────────────────────────────────────────────────────
// Network message processing
// ──────────────────────────────────────────────────────────────

func (g *Game) processNetwork() {
	if g.conn == nil {
		return
	}
	for {
		data, ok := g.conn.TryReceive()
		if !ok {
			break
		}
		g.handleServerMsg(data)
	}
}

func (g *Game) handleServerMsg(data []byte) {
	var msg ServerMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}

	switch msg.Type {
	case "auth_ok":
		g.localID = msg.ID
		g.localGralats = msg.GralatN
		g.localPlaytime = msg.Playtime
		g.isAdmin = msg.IsAdmin
		g.sessionStart = time.Now()
		// Restore the map instance the player logged out on. The stored X/Y are
		// only meaningful on that map, so this must happen before the position is
		// applied — loadGMap/loadMap reposition the character themselves.
		if msg.Map != "" && msg.Map != g.activeGMap && msg.Map != g.currentMapName {
			if strings.HasSuffix(strings.ToLower(msg.Map), ".gmap") {
				g.loadGMap(msg.Map)
			} else {
				g.loadMap(msg.Map, false)
			}
		}
		if g.localChar != nil {
			// Only use the server's stored position when no explicit spawn is configured.
			if Cfg.SpawnX == 0 && Cfg.SpawnY == 0 {
				g.localChar.X, g.localChar.Y = msg.X, msg.Y
				g.localChar.TargetX, g.localChar.TargetY = msg.X, msg.Y
			}
		}
		if msg.Body != "" || msg.Head != "" || msg.Hat != "" || msg.Shield != "" || msg.Sword != "" {
			g.cosmeticMenu.SetByFilenames(msg.Body, msg.Head, msg.Hat, msg.Shield, msg.Sword)
		}
		g.sendCosmetics()
		// Re-announce the current map so the server tracks which map this client
		// is on. In GMAP mode activeGMap holds the name; in TMX mode currentMapName.
		if g.conn != nil {
			if g.activeGMap != "" {
				g.conn.SendJSON(map[string]string{"type": "change_map", "map": g.activeGMap})
			} else if g.currentMapName != "" {
				g.conn.SendJSON(map[string]string{"type": "change_map", "map": g.currentMapName})
			}
		}
		g.chat.AddMessage("", fmt.Sprintf("Connected as %s", msg.Name), true)

	case "auth_error":
		g.chat.AddMessage("", "Auth error: "+msg.Msg, true)
		g.disconnect()
		g.state = StateMainMenu

	case "state":
		g.mu.Lock()

		// Remote players
		seen := make(map[string]bool)
		for _, p := range msg.Players {
			if p.ID == g.localID {
				continue
			}
			seen[p.ID] = true
			if ch, ok := g.otherPlayers[p.ID]; ok {
				// Correct to server-authoritative position and update dead reckoning.
				ch.TargetX, ch.TargetY = p.X, p.Y
				ch.Dir = p.Dir
				ch.Moving = p.Moving
				// Recompute velocity from authoritative state so the next
				// dead-reckoning frame starts from the corrected baseline.
				drSpeed := Cfg.PlayerSpeed
				if p.Mounted {
					drSpeed = Cfg.MountedSpeed
				}
				ch.velX, ch.velY = remoteVel(p.Dir, p.Moving, drSpeed)
				ch.Gralats = p.Gralats
				ch.Playtime = p.Playtime
				if p.MaxHP > 0 {
					ch.MaxHP = p.MaxHP
					ch.HP = p.HP
				}
				ch.SetCosmetics(p.Body, p.Head, p.Hat, p.Shield, p.Sword)
				// Mount / ride sync
				ch.Mounted = p.Mounted
				if p.Mounted || p.AnimState == AnimRide {
					ch.AnimState = AnimRide
					ch.Mounted = true
				} else if ch.AnimState == AnimRide && !p.Mounted {
					ch.AnimState = AnimIdle
					ch.Mounted = false
				}
				// Sword
				if p.AnimState == AnimSword && ch.AnimState != AnimSword {
					ch.StartSword()
				}
				// Sit
				if p.AnimState == AnimSit {
					ch.AnimState = AnimSit
				} else if ch.AnimState == AnimSit && p.AnimState != AnimSit {
					ch.AnimState = AnimIdle
				}
				// Grab
				if p.AnimState == AnimGrab && ch.AnimState != AnimGrab {
					ch.AnimState = AnimGrab
				} else if ch.AnimState == AnimGrab && p.AnimState != AnimGrab {
					ch.AnimState = AnimIdle
				}
				// Dead / respawn
				if p.AnimState == AnimDead && ch.AnimState != AnimDead {
					ch.AnimState = AnimDead
					ch.hurtTimer = 0
					ch.knockTimer = 0
				} else if ch.AnimState == AnimDead && p.AnimState != AnimDead {
					ch.AnimState = AnimIdle
					ch.X, ch.Y = p.X, p.Y
					ch.TargetX, ch.TargetY = p.X, p.Y
				}
				// Push
				if p.AnimState == AnimPush && ch.AnimState != AnimPush && ch.AnimState != AnimDead {
					ch.AnimState = AnimPush
				} else if ch.AnimState == AnimPush && p.AnimState != AnimPush {
					ch.AnimState = AnimIdle
				}
				// Hurt
				if p.AnimState == AnimHurt && ch.AnimState != AnimHurt && ch.AnimState != AnimDead {
					ch.hurtTimer = 0.30
					ch.AnimState = AnimHurt
				}
				// Item anims
				itemAnims := map[string]bool{
					AnimClassicJuggle: true, AnimPompoms: true, AnimJuggle: true, AnimHatTrick: true,
				}
				if itemAnims[p.AnimState] && !itemAnims[ch.AnimState] {
					ch.AnimState = p.AnimState
				} else if itemAnims[ch.AnimState] && !itemAnims[p.AnimState] {
					ch.AnimState = AnimIdle
				}
			} else {
				ch := NewCharacter(g.bodyImg, g.headImg, p.X, p.Y, p.Name, false, 0)
				ch.Gralats = p.Gralats
				ch.Playtime = p.Playtime
				ch.Mounted = p.Mounted
				if p.MaxHP > 0 {
					ch.MaxHP = p.MaxHP
					ch.HP = p.HP
				}
				if p.Mounted {
					ch.AnimState = AnimRide
				}
				drSpeed := Cfg.PlayerSpeed
				if p.Mounted {
					drSpeed = Cfg.MountedSpeed
				}
				ch.velX, ch.velY = remoteVel(p.Dir, p.Moving, drSpeed)
				ch.SetCosmetics(p.Body, p.Head, p.Hat, p.Shield, p.Sword)
				g.otherPlayers[p.ID] = ch
			}
		}
		for id := range g.otherPlayers {
			if !seen[id] {
				delete(g.otherPlayers, id)
			}
		}

		// NPCs
		seenNPC := make(map[string]bool)
		for _, n := range msg.NPCs {
			seenNPC[n.ID] = true
			drSpeed := npcDeadReckonSpeed(n.NPCType)
			if ch, ok := g.npcs[n.ID]; ok {
				ch.missedTicks = 0
				ch.TargetX, ch.TargetY = n.X, n.Y
				ch.Dir = n.Dir
				ch.Moving = n.Moving
				ch.velX, ch.velY = remoteVel(n.Dir, n.Moving, drSpeed)
				ch.HP = n.HP
				ch.MaxHP = n.MaxHP
				if n.AnimState == "dead" && ch.AnimState != AnimDead {
					ch.AnimState = AnimDead
				} else if n.AnimState == "" && ch.AnimState == AnimDead {
					ch.AnimState = AnimIdle
				}
			} else {
				ch := NewCharacter(g.bodyImg, g.headImg, n.X, n.Y, n.Name, true, n.NPCType)
				ch.HP = n.HP
				ch.MaxHP = n.MaxHP
				if n.AnimState == "dead" {
					ch.AnimState = AnimDead
				}
				ch.velX, ch.velY = remoteVel(n.Dir, n.Moving, drSpeed)
				g.npcs[n.ID] = ch
			}
		}
		// Keep NPCs alive for up to 30 missed ticks (~500ms) to absorb dropped packets.
		for id, ch := range g.npcs {
			if !seenNPC[id] {
				ch.missedTicks++
				if ch.missedTicks > 30 {
					delete(g.npcs, id)
				}
			}
		}
		g.mu.Unlock()

		// World gralats
		g.grMu.Lock()
		g.worldGralats = msg.Gralats
		g.grMu.Unlock()

		// World items — always update (nil clears the list when off main map)
		g.mu.Lock()
		g.worldItems = msg.WorldItems
		g.mu.Unlock()
		g.adminMenu.SetWorldItems(msg.WorldItems)

	case "chat":
		g.chat.AddMessage(msg.From, msg.Msg, false)
		g.mu.Lock()
		for _, p := range g.otherPlayers {
			if p.Name == msg.From {
				p.SetChatMsg(msg.Msg)
				if code := containsEmoji(msg.Msg); code != "" {
					if img := emojiImageFor(code); img != nil {
						p.SetEmoji(img)
					}
				}
				break
			}
		}
		g.mu.Unlock()

	case "system":
		g.chat.AddMessage("", msg.Msg, true)

	case "gralat_update":
		g.localGralats = msg.GralatN

	case "npc_dialog":
		g.npcDialog = msg.Msg
		g.npcGralatN = msg.GralatN
		if msg.GralatN > 0 {
			g.localGralats += msg.GralatN
		}

	case "npc_damage":
		g.mu.Lock()
		if npc, ok := g.npcs[msg.NPCID]; ok {
			npc.HP = msg.HP
			if msg.Killed {
				npc.AnimState = AnimDead
			}
		}
		g.mu.Unlock()
		if msg.Killed {
			g.chat.AddMessage("", "You defeated an enemy!", true)
		}

	case "mount_ok":
		if g.localChar != nil {
			g.localChar.Mounted = true
			g.localChar.AnimState = AnimRide
		}

	case "dismount_ok":
		if g.localChar != nil {
			g.localChar.Mounted = false
			g.localChar.AnimState = AnimIdle
		}

	case "pvp_damage":
		if g.localChar != nil && g.localChar.AnimState != AnimDead {
			newHP := msg.HP
			if newHP < 0 {
				newHP = 0
			}
			if newHP > g.localMaxHP {
				newHP = g.localMaxHP
			}
			if newHP < g.localHP {
				dmg := g.localHP - newHP
				g.localHP = newHP
				if g.localHP == 0 {
					g.localChar.AnimState = AnimDead
					g.localChar.hurtTimer = 0
					g.localChar.knockTimer = 0
					g.localChar.knockVX = 0
					g.localChar.knockVY = 0
					g.chat.AddMessage("", "You have been killed!", true)
				} else {
					g.localChar.SetHurt(msg.AtkX, msg.AtkY, dmg)
				}
			}
		}

	case "inventory_data":
		g.localInventory = msg.Inventory
		if g.inventoryMenu != nil {
			g.inventoryMenu.SetItems(g.localInventory)
		}

	case "use_item_ok":
		if msg.PlayerID != g.localID {
			g.mu.Lock()
			if ch, ok := g.otherPlayers[msg.PlayerID]; ok {
				ch.AnimState = msg.AnimSt
			}
			g.mu.Unlock()
		}

	case "buy_result":
		if msg.Success {
			g.chat.AddMessage("", msg.Msg, true)
			if msg.GralatN > 0 {
				g.localGralats = msg.GralatN
			}
		} else {
			g.chat.AddMessage("", "Error: "+msg.Msg, true)
		}

	case "load_map":
		if msg.Map != "" {
			name := msg.Map
			if len(name) >= 5 && name[len(name)-5:] == ".gmap" {
				g.loadGMap(name)
			} else {
				g.activeGMap = ""
				g.loadMap(name, false)
			}
		}

	// ── Friends ───────────────────────────────────────────────
	case "friend_list":
		g.friends = msg.Friends
		g.friendRequests = msg.Requests
		if g.socialPanel != nil {
			g.socialPanel.SetFriends(g.friends, g.friendRequests)
		}

	case "friend_request":
		g.chat.AddMessage("", fmt.Sprintf("[Friend] %s sent you a friend request!", msg.From), true)
		if g.conn != nil {
			// Request an updated list
			g.conn.SendJSON(map[string]string{"type": "friend_list"})
		}

	case "friend_result":
		g.chat.AddMessage("", "[Friend] "+msg.Msg, true)

	// ── Guilds ────────────────────────────────────────────────
	case "guild_info":
		g.myGuild = msg.Guild
		if g.socialPanel != nil {
			g.socialPanel.SetGuild(g.myGuild)
		}

	case "guild_list":
		g.guildList = msg.Guilds
		if g.socialPanel != nil {
			g.socialPanel.SetGuildList(g.guildList)
		}

	case "guild_result":
		if msg.Success {
			g.chat.AddMessage("", "[Guild] "+msg.Msg, true)
		} else {
			g.chat.AddMessage("", "[Guild] Error: "+msg.Msg, true)
		}

	// ── Quests ────────────────────────────────────────────────
	case "quest_list":
		g.quests = msg.Quests
		if g.socialPanel != nil {
			g.socialPanel.SetQuests(g.quests)
		}

	case "quest_update":
		for i, q := range g.quests {
			if q.ID == msg.QuestID {
				g.quests[i].Progress = msg.Progress
				break
			}
		}
		if g.socialPanel != nil {
			g.socialPanel.SetQuests(g.quests)
		}
		g.chat.AddMessage("", fmt.Sprintf("[Quest] Progress: %d/%d", msg.Progress, msg.Required), true)

	case "quest_complete":
		for i, q := range g.quests {
			if q.ID == msg.QuestID {
				g.quests[i].Completed = true
				g.quests[i].Progress = g.quests[i].Required
				break
			}
		}
		if g.socialPanel != nil {
			g.socialPanel.SetQuests(g.quests)
		}
		g.chat.AddMessage("", fmt.Sprintf("[Quest Complete!] %s — Reward: %d gralats", msg.Name, msg.GralatN), true)

	case "quest_result":
		g.chat.AddMessage("", "[Quest] "+msg.Msg, true)
	}
}

