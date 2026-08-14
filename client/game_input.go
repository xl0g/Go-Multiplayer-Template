package main

import (
	"fmt"
	"image"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
)

// ──────────────────────────────────────────────────────────────
// Focus helpers
// ──────────────────────────────────────────────────────────────

// uiHasFocus returns true when any text input widget currently has keyboard
// focus (admin menu fields, etc.). While this is true, all global hotkeys
// (I, P, X, A, Tab, Esc, …) must be suppressed so typed characters don't
// accidentally trigger game actions.
func (g *Game) uiHasFocus() bool {
	return g.adminMenu.HasFocus()
}

// ──────────────────────────────────────────────────────────────
// Virtual button geometry
// ──────────────────────────────────────────────────────────────

// virtualGrabRect returns the screen rect of the grab (glove) virtual button.
func (g *Game) virtualGrabRect() (x, y, w, h int) {
	return screenW - 170, screenH - 64, 50, 50
}

// virtualSwordRect returns the screen rect of the sword virtual button.
func (g *Game) virtualSwordRect() (x, y, w, h int) {
	return screenW - 114, screenH - 64, 50, 50
}

// isOverVirtualButton reports whether the cursor is on any virtual button.
func (g *Game) isOverVirtualButton(mx, my int) bool {
	gx, gy, gw, gh := g.virtualGrabRect()
	if mx >= gx && mx < gx+gw && my >= gy && my < gy+gh {
		return true
	}
	sx, sy, sw, sh := g.virtualSwordRect()
	return mx >= sx && mx < sx+sw && my >= sy && my < sy+sh
}

// ──────────────────────────────────────────────────────────────
// Action helpers
// ──────────────────────────────────────────────────────────────

// triggerSword starts a sword swing for the local player.
func (g *Game) triggerSword() {
	if g.localChar == nil || g.chat.IsOpen {
		return
	}
	if g.localChar.StartSword() {
		g.swordHitSent = false
		if g.conn != nil {
			g.conn.SendJSON(map[string]interface{}{
				"type": "anim_state",
				"anim": AnimSword,
			})
		}
	}
}

// ──────────────────────────────────────────────────────────────
// Playing update (main game-loop tick)
// ──────────────────────────────────────────────────────────────

func (g *Game) updatePlaying(dt float64) error {
	if g.localChar == nil {
		return nil
	}

	// Panel menu — consumes mouse clicks when active.
	g.panelMenu.Update(dt)
	if req := g.panelMenu.RequestMap; req != "" {
		g.panelMenu.RequestMap = ""
		g.prevMapName = ""
		g.activeGMap = ""
		g.loadMap(req, false)
		g.mapSwitchCooldown = 3.0
	}
	if req := g.panelMenu.RequestGMap; req != "" {
		g.panelMenu.RequestGMap = ""
		g.prevMapName = ""
		g.loadGMap(req)
	}
	if g.panelMenu.RequestInventory {
		g.panelMenu.RequestInventory = false
		g.inventoryMenu.Open()
	}

	// Social panel update (Friends / Guilds / Quests)
	if g.socialPanel != nil && g.panelMenu != nil {
		activeSub := g.panelMenu.ActiveSocial()
		if activeSub != "" {
			subTop := g.panelMenu.SocialPanelTop()
			if action := g.socialPanel.Update(activeSub, subTop); action != nil && g.conn != nil {
				msg := map[string]interface{}{"type": action.Type}
				for k, v := range action.Payload {
					msg[k] = v
				}
				g.conn.SendJSON(msg)
				// Refresh relevant data after action
				switch action.Type {
				case "friend_add", "friend_accept", "friend_remove":
					g.conn.SendJSON(map[string]string{"type": "friend_list"})
				case "guild_create", "guild_join", "guild_leave":
					g.conn.SendJSON(map[string]string{"type": "guild_info"})
					g.conn.SendJSON(map[string]string{"type": "guild_list"})
				case "quest_start":
					g.conn.SendJSON(map[string]string{"type": "quest_list"})
				case "guild_list":
					// already sent above
				}
			}
		}
	}

	// F3 → toggle debug overlay
	if inpututil.IsKeyJustPressed(ebiten.KeyF3) {
		g.debugOverlay = !g.debugOverlay
	}

	// Mouse wheel → zoom (clamp to ±1 per frame so fast scrolling stays gradual)
	_, wy := ebiten.Wheel()
	if wy != 0 {
		if wy > 1 {
			wy = 1
		} else if wy < -1 {
			wy = -1
		}
		g.zoom *= 1.0 + wy*0.07
		if g.zoom < Cfg.ZoomMin {
			g.zoom = Cfg.ZoomMin
		}
		if g.zoom > Cfg.ZoomMax {
			g.zoom = Cfg.ZoomMax
		}
	}

	g.processNetwork()

	// Inventory menu
	if g.inventoryMenu.IsVisible() {
		g.inventoryMenu.Update()
		if id := g.inventoryMenu.TakeUsed(); id != "" {
			g.useInventoryItem(id)
		}
	}

	// Admin menu
	if g.adminMenu.IsVisible() {
		g.adminMenu.Update()
		if req := g.adminMenu.SpawnReq; req != nil {
			g.adminMenu.SpawnReq = nil
			if g.conn != nil && g.localChar != nil {
				req.X = g.localChar.X
				req.Y = g.localChar.Y
				g.conn.SendJSON(map[string]interface{}{
					"type":    "admin_spawn_world_item",
					"name":    req.Name,
					"sprite":  req.SpritePath,
					"x":       req.X,
					"y":       req.Y,
					"price":   req.Price,
					"item_id": req.ItemID,
				})
			}
		}
		if id := g.adminMenu.RemoveID; id != "" {
			g.adminMenu.RemoveID = ""
			if g.conn != nil {
				g.conn.SendJSON(map[string]string{
					"type": "admin_remove_world_item",
					"id":   id,
				})
			}
		}
		// Drain the debug panel's request queue. It builds whole messages itself,
		// so nothing here needs to know what each one means.
		if len(g.adminMenu.Out) > 0 {
			if g.conn != nil {
				for _, msg := range g.adminMenu.Out {
					g.conn.SendJSON(msg)
				}
			}
			g.adminMenu.Out = g.adminMenu.Out[:0]
		}

		// The debug panel is modal for the keyboard. Its shortcuts (B/K/V/P/G/H/
		// T/S/F/E/M/R and the arrows) overlap gameplay keys, so returning here
		// stops the game from also toggling the profile, mounting, swinging the
		// sword or walking the player around while the panel is open.
		if inpututil.IsKeyJustPressed(ebiten.KeyEscape) {
			if g.adminMenu.HasFocus() {
				g.adminMenu.blurAll() // first Esc leaves the text field
			} else {
				g.adminMenu.Close()
			}
		} else if inpututil.IsKeyJustPressed(ebiten.KeyTab) && !g.adminMenu.HasFocus() {
			g.adminMenu.Close()
		}
		return nil
	}

	// Escape: close overlays in order.
	if inpututil.IsKeyJustPressed(ebiten.KeyEscape) && !g.chat.IsOpen {
		if g.worldItemDialog != nil {
			g.worldItemDialog = nil
			return nil
		}
		if g.inventoryMenu.IsVisible() {
			g.inventoryMenu.Close()
			return nil
		}
		if g.adminMenu.IsVisible() {
			g.adminMenu.Close()
			return nil
		}
		if g.signDialog != "" || g.npcDialog != "" {
			g.signDialog = ""
			g.npcDialog = ""
			return nil
		}
		if g.viewedPlayer != nil {
			g.viewedPlayer = nil
			return nil
		}
		if g.profileOpen {
			g.profileOpen = false
			return nil
		}
	}

	// Profile toggle
	if inpututil.IsKeyJustPressed(ebiten.KeyP) && !g.chat.IsOpen && !g.uiHasFocus() {
		g.profileOpen = !g.profileOpen
		g.viewedPlayer = nil
	}

	// I key: toggle inventory
	if inpututil.IsKeyJustPressed(ebiten.KeyI) && !g.chat.IsOpen && !g.uiHasFocus() {
		g.inventoryMenu.Toggle()
		g.adminMenu.Close()
	}

	// Tab key: toggle the admin / debug panel (admin accounts only).
	if inpututil.IsKeyJustPressed(ebiten.KeyTab) && !g.chat.IsOpen && !g.uiHasFocus() {
		if g.isAdmin {
			g.adminMenu.Toggle()
			g.inventoryMenu.Close()
		} else {
			// Say so rather than doing nothing: a silent no-op is indistinguishable
			// from a broken key, and the fix (granting admin) is not guessable.
			g.chat.AddMessage("", "Admin only. Grant it with: ./game-server -setadmin "+g.localName, true)
		}
	}

	// ── Virtual button clicks (must be before handlePlayerClick) ──
	if inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonLeft) && !g.chat.IsOpen {
		mx, my := ebiten.CursorPosition()
		// Grab button: hold to grab
		gx, gy, gw, gh := g.virtualGrabRect()
		if mx >= gx && mx < gx+gw && my >= gy && my < gy+gh {
			g.grabBtnHeld = true
		}
		// Sword button: single trigger
		sx, sy, sw, sh := g.virtualSwordRect()
		if mx >= sx && mx < sx+sw && my >= sy && my < sy+sh {
			g.triggerSword()
		}
	}
	// Release grab button when mouse is released
	if !ebiten.IsMouseButtonPressed(ebiten.MouseButtonLeft) {
		g.grabBtnHeld = false
	}

	// Left-click: world item popup / buy / select player
	if inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonLeft) &&
		!g.chat.IsOpen && !g.cosmeticMenu.IsVisible() &&
		g.signDialog == "" && g.npcDialog == "" {
		mx, my := ebiten.CursorPosition()
		if !g.isOverVirtualButton(mx, my) {
			// Viewed player profile buttons (Add Friend / Invite to Guild)
			if g.viewedPlayer != nil {
				if g.handleViewedProfileClick(mx, my) {
					// click consumed
				}
			} else if g.worldItemDialog != nil {
				if g.handleWorldItemDialogClick(mx, my) {
					// click was consumed by the dialog
				} else {
					g.worldItemDialog = nil
				}
			} else if wi := g.worldItemAtScreen(mx, my); wi != nil {
				// Clicked directly on a world item sprite → open popup
				g.worldItemDialog = wi
			} else {
				g.handlePlayerClick()
			}
		}
	}

	// F key: interact
	if inpututil.IsKeyJustPressed(ebiten.KeyF) && !g.chat.IsOpen && !g.uiHasFocus() &&
		!g.inventoryMenu.IsVisible() && !g.adminMenu.IsVisible() {
		if g.signDialog != "" || g.npcDialog != "" {
			g.signDialog = ""
			g.npcDialog = ""
		} else if g.nearWorldItem != nil && g.nearWorldItem.Price > 0 {
			if g.conn != nil {
				g.conn.SendJSON(map[string]string{
					"type": "buy_world_item",
					"id":   g.nearWorldItem.ID,
				})
			}
		} else if g.activeGMap != "" {
			if text, ok := g.chunkMgr.NearbySign(g.localChar.X, g.localChar.Y); ok {
				g.signDialog = text
			}
		} else if g.gameMap != nil {
			if sign := g.gameMap.NearbySign(g.localChar.X, g.localChar.Y); sign != "" {
				g.signDialog = sign
			} else if g.nearNPCID != "" && g.nearNPCType != NPCTypeHorse && g.conn != nil {
				g.conn.SendJSON(map[string]string{
					"type":   "talk_npc",
					"npc_id": g.nearNPCID,
				})
			}
		}
	}

	// X key: sword swing
	if inpututil.IsKeyJustPressed(ebiten.KeyX) && !g.chat.IsOpen && !g.uiHasFocus() {
		g.triggerSword()
	}

	// Q key (AZERTY physical A): grab — hold to grab
	// ebiten.KeyQ = scancode Q = physical "A" on a French AZERTY keyboard.
	if !g.chat.IsOpen && !g.uiHasFocus() && g.localChar.AnimState != AnimDead {
		grabHeld := ebiten.IsKeyPressed(ebiten.KeyQ) || g.grabBtnHeld
		if grabHeld {
			if g.localChar.AnimState != AnimGrab {
				g.localChar.AnimState = AnimGrab
				if g.conn != nil {
					g.conn.SendJSON(map[string]interface{}{
						"type": "anim_state",
						"anim": AnimGrab,
					})
				}
			}
		} else if g.localChar.AnimState == AnimGrab {
			g.localChar.AnimState = AnimIdle
			if g.conn != nil {
				g.conn.SendJSON(map[string]interface{}{
					"type": "anim_state",
					"anim": AnimIdle,
				})
			}
		}
	}

	// R key: mount / dismount
	if inpututil.IsKeyJustPressed(ebiten.KeyR) && !g.chat.IsOpen && !g.uiHasFocus() {
		if g.localChar.Mounted {
			if g.conn != nil {
				g.conn.SendJSON(map[string]string{"type": "dismount"})
			}
			g.localChar.Mounted = false
			g.localChar.AnimState = AnimIdle
		} else if g.nearNPCType == NPCTypeHorse && g.nearNPCID != "" && g.conn != nil {
			g.conn.SendJSON(map[string]string{
				"type":   "mount_npc",
				"npc_id": g.nearNPCID,
			})
		}
	}

	// Cosmetic menu — consumes all input while open.
	if g.cosmeticMenu.IsVisible() {
		g.cosmeticMenu.Update()
		if g.cosmeticMenu.TakeChanged() {
			g.sendCosmetics()
		}
		return nil
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyC) && !g.chat.IsOpen && !g.uiHasFocus() {
		g.cosmeticMenu.Open()
		return nil
	}

	// T key: open chat
	if inpututil.IsKeyJustPressed(ebiten.KeyT) && !g.chat.IsOpen && !g.uiHasFocus() &&
		g.signDialog == "" && g.npcDialog == "" {
		g.chat.IsOpen = true
	}

	// Chat update
	if msg, sent := g.chat.Update(); sent {
		g.handleChatMessage(msg)
	}

	// Movement — blocked while chat/dialogs/UI fields are open
	if !g.chat.IsOpen && g.signDialog == "" && g.npcDialog == "" && !g.uiHasFocus() {
		g.handleMovement(dt)
	}

	// Animate all entities
	g.localChar.Update(dt)
	g.mu.Lock()
	for _, ch := range g.otherPlayers {
		ch.Update(dt)
	}
	for _, npc := range g.npcs {
		npc.Update(dt)
	}
	g.mu.Unlock()

	// Clear stale viewedPlayer if they disconnected
	if g.viewedPlayer != nil {
		g.mu.Lock()
		found := false
		for _, p := range g.otherPlayers {
			if p == g.viewedPlayer {
				found = true
				break
			}
		}
		g.mu.Unlock()
		if !found {
			g.viewedPlayer = nil
		}
	}

	// Nearest NPC and world item.
	//
	// This used to be gated on currentMapName being the old default TMX map, which
	// silently disabled talking to and mounting NPCs on every other map — and
	// stayed disabled after returning from a building, since currentMapName keeps
	// the interior's name. The server only ever sends NPCs on the player's own map
	// instance, so no map check belongs here.
	g.nearNPCID, g.nearNPCType = g.nearestNPC()
	g.nearWorldItem = g.findNearWorldItem()

	// Sword hit detection
	if !g.swordHitSent && g.localChar.SwordJustActivated() {
		g.localChar.MarkSwordHitDone()
		g.swordHitSent = true
		g.checkSwordHit()
	}

	// Advance animated tiles
	if g.gameMap != nil {
		g.gameMap.Update(dt)
	}

	// Advance chunk streaming (GMAP mode).
	// viewRadius = how many chunks to load in each direction from the player chunk.
	// chunksX/Y is the full count visible, so the radius is half that (rounded up).
	if g.activeGMap != "" {
		chunksX := int(math.Ceil(float64(screenW) / g.zoom / float64(chunkPixelW)))
		chunksY := int(math.Ceil(float64(screenH) / g.zoom / float64(chunkPixelH)))
		viewRadius := (chunksX + 1) / 2
		if r := (chunksY + 1) / 2; r > viewRadius {
			viewRadius = r
		}
		if viewRadius < Cfg.ChunkRadius {
			viewRadius = Cfg.ChunkRadius
		}
		g.chunkMgr.Update(g.localChar.X, g.localChar.Y, viewRadius)
	}

	// Gralat auto-collect
	g.checkGralatPickup()

	// Map transition cooldown + triggers
	if g.mapSwitchCooldown > 0 {
		g.mapSwitchCooldown -= dt
	}
	if g.mapSwitchCooldown <= 0 {
		c := g.localChar
		if g.activeGMap != "" {
			// Warp links from chunk NW data (doors, exits).
			// Use same 80×80 detection box centred on body.
			if lnk, ok := g.chunkMgr.WarpAt(c.X-24, c.Y-24, 80, 80); ok {
				dest := lnk.DestMap
				// Ignore inter-chunk LINKs — in the original Graal engine each
				// .nw file had explicit LINK entries to its neighbours.  In GMAP
				// mode the chunk-streaming system handles movement between chunks
				// automatically, so we only follow LINKs to *outside* maps
				// (buildings, dungeons, etc.) that are not part of this GMAP.
				if g.chunkMgr.IsPartOfGMap(dest) {
					// neighbour chunk — let the player walk through normally
				} else {
					destX := lnk.DestX * chunkTileW
					destY := lnk.DestY * chunkTileH
					if strings.HasSuffix(strings.ToLower(dest), ".gmap") {
						g.loadGMap(dest)
					} else {
						// Save GMAP context (with loaded chunks) to restore on exit.
						g.prevGMap = g.activeGMap
						g.prevChunkMgr = g.chunkMgr
						g.prevGMapX = c.X
						g.prevGMapY = c.Y
						g.activeGMap = ""
						g.loadMap(dest, false)
					}
					if g.localChar != nil {
						g.localChar.X = destX - float64(frameW)/2
						g.localChar.Y = destY - float64(frameH)/2
						g.localChar.TargetX = g.localChar.X
						g.localChar.TargetY = g.localChar.Y
					}
				}
			}
		} else if g.gameMap != nil {
			// Warp detection box (80×80) centred on body centre (c.X+16, c.Y+16).
			gaX := c.X - 24
			gaY := c.Y - 24

			// isGMapChunk reports whether a destination belongs to the saved GMAP.
			isGMapChunk := func(dest string) bool {
				return g.prevGMap != "" && g.prevChunkMgr != nil && g.prevChunkMgr.IsPartOfGMap(dest)
			}

			if target := g.gameMap.SwitchmapAt(gaX, gaY, 80, 80); target != "" {
				if isGMapChunk(target) {
					g.restorePrevGMap()
				} else if strings.HasSuffix(strings.ToLower(target), ".gmap") {
					g.loadGMap(target)
				} else {
					g.loadMap(target, true)
				}
			} else if lnk, ok := g.gameMap.WarpLinkAt(c.X, c.Y, float64(frameW), float64(frameH)); ok {
				dest := lnk.DestMap
				if isGMapChunk(dest) {
					g.restorePrevGMap()
				} else {
					destX := lnk.DestX*float64(g.gameMap.TileW) - float64(frameW)/2
					destY := lnk.DestY*float64(g.gameMap.TileH) - float64(frameH)/2
					fmt.Printf("[WARP] %s → %s  dest=(%.1f,%.1f)\n", g.currentMapName, dest, destX, destY)
					if strings.HasSuffix(strings.ToLower(dest), ".gmap") {
						g.loadGMap(dest)
					} else {
						g.loadMap(dest, false)
					}
					if g.localChar != nil {
						g.localChar.X = destX
						g.localChar.Y = destY
						g.localChar.TargetX = destX
						g.localChar.TargetY = destY
					}
				}
			} else if g.gameMap.OnExitTile(gaX, gaY, 80, 80) {
				if g.prevGMap != "" {
					g.restorePrevGMap()
				} else if g.prevMapName != "" {
					g.loadMap(g.prevMapName, true)
				}
			}
		}
	}

	// Sync AnimState changes to server
	if g.conn != nil && g.localChar.AnimState != g.lastSentAnim {
		g.lastSentAnim = g.localChar.AnimState
		g.conn.SendJSON(map[string]interface{}{
			"type":    "anim_state",
			"anim":    g.localChar.AnimState,
			"mounted": g.localChar.Mounted,
		})
	}

	return nil
}

// ──────────────────────────────────────────────────────────────
// Movement
// ──────────────────────────────────────────────────────────────

func (g *Game) handleMovement(dt float64) {
	c := g.localChar

	// isMapBlocked checks collision against whichever map backend is active.
	isMapBlocked := func(x, y, w, h float64) bool {
		if g.activeGMap != "" {
			return g.chunkMgr.IsBlocked(x, y, w, h)
		}
		if g.gameMap != nil {
			return g.gameMap.IsBlocked(x, y, w, h)
		}
		return false
	}

	// Apply knockback with tile collision (local player only).
	if c.knockTimer > 0 && (c.knockVX != 0 || c.knockVY != 0) {
		kx := c.knockVX * dt
		ky := c.knockVY * dt
		if !g.noclip {
			if !isMapBlocked(c.X+kx, c.Y, float64(frameW), float64(frameH)) {
				c.X += kx
			}
			if !isMapBlocked(c.X, c.Y+ky, float64(frameW), float64(frameH)) {
				c.Y += ky
			}
		} else {
			c.X += kx
			c.Y += ky
		}
	}

	// Block movement input while being knocked back.
	if c.knockTimer > 0 {
		c.Moving = false
		return
	}

	// Block movement while swinging sword.
	if c.AnimState == AnimSword {
		c.Moving = false
		return
	}

	// Block movement while grabbing.
	if c.AnimState == AnimGrab {
		c.Moving = false
		return
	}

	// Dead: any movement key respawns the player.
	if c.AnimState == AnimDead {
		anyKey := ebiten.IsKeyPressed(ebiten.KeyUp) || ebiten.IsKeyPressed(ebiten.KeyW) ||
			ebiten.IsKeyPressed(ebiten.KeyDown) || ebiten.IsKeyPressed(ebiten.KeyS) ||
			ebiten.IsKeyPressed(ebiten.KeyLeft) || ebiten.IsKeyPressed(ebiten.KeyA) ||
			ebiten.IsKeyPressed(ebiten.KeyRight) || ebiten.IsKeyPressed(ebiten.KeyD)
		if anyKey {
			g.localHP = g.localMaxHP
			c.AnimState = AnimIdle
		}
		c.Moving = false
		return
	}

	anyKey := ebiten.IsKeyPressed(ebiten.KeyUp) || ebiten.IsKeyPressed(ebiten.KeyW) ||
		ebiten.IsKeyPressed(ebiten.KeyDown) || ebiten.IsKeyPressed(ebiten.KeyS) ||
		ebiten.IsKeyPressed(ebiten.KeyLeft) || ebiten.IsKeyPressed(ebiten.KeyA) ||
		ebiten.IsKeyPressed(ebiten.KeyRight) || ebiten.IsKeyPressed(ebiten.KeyD)

	// Cancel sit when any movement key is pressed.
	if c.AnimState == AnimSit && anyKey {
		c.AnimState = AnimIdle
	}
	if c.AnimState == AnimSit {
		c.Moving = false
		return
	}

	// Cancel item animations when the player starts moving.
	if anyKey && (c.AnimState == AnimClassicJuggle || c.AnimState == AnimPompoms || c.AnimState == AnimJuggle) {
		c.AnimState = AnimIdle
		if g.conn != nil {
			g.conn.SendJSON(map[string]interface{}{"type": "anim_state", "anim": AnimIdle})
		}
	}

	c.Moving = false
	dx, dy := 0.0, 0.0

	if ebiten.IsKeyPressed(ebiten.KeyUp) || ebiten.IsKeyPressed(ebiten.KeyW) {
		dy = -1; c.Dir = 0; c.Moving = true
	}
	if ebiten.IsKeyPressed(ebiten.KeyDown) || ebiten.IsKeyPressed(ebiten.KeyS) {
		dy = 1; c.Dir = 2; c.Moving = true
	}
	if ebiten.IsKeyPressed(ebiten.KeyLeft) || ebiten.IsKeyPressed(ebiten.KeyA) {
		dx = -1; c.Dir = 1; c.Moving = true
	}
	if ebiten.IsKeyPressed(ebiten.KeyRight) || ebiten.IsKeyPressed(ebiten.KeyD) {
		dx = 1; c.Dir = 3; c.Moving = true
	}

	if dx != 0 && dy != 0 {
		dx *= 0.7071
		dy *= 0.7071
	}

	speed := Cfg.PlayerSpeed
	if c.Mounted {
		speed = Cfg.MountedSpeed
	}

	newX := c.X + dx*speed*dt
	newY := c.Y + dy*speed*dt

	pushedIntoWall := false
	hasMap := g.activeGMap != "" || g.gameMap != nil
	if hasMap && !g.noclip {
		blockedX := dx != 0 && (isMapBlocked(newX, c.Y, float64(frameW), float64(frameH)) ||
			g.isBlockedByWorldItem(newX, c.Y, float64(frameW), float64(frameH)))
		blockedY := dy != 0 && (isMapBlocked(c.X, newY, float64(frameW), float64(frameH)) ||
			g.isBlockedByWorldItem(c.X, newY, float64(frameW), float64(frameH)))

		if !blockedX {
			c.X = newX
		}
		if !blockedY {
			c.Y = newY
		}

		if c.Moving {
			if math.Abs(dx) >= math.Abs(dy) {
				pushedIntoWall = blockedX
			} else {
				pushedIntoWall = blockedY
			}
		}
	} else {
		c.X = newX
		c.Y = newY
	}

	// Push animation
	if c.AnimState != AnimRide {
		if pushedIntoWall {
			g.pushTimer += dt
			if g.pushTimer >= 1.5 {
				c.AnimState = AnimPush
			}
		} else {
			g.pushTimer = 0
			if c.AnimState == AnimPush {
				c.AnimState = AnimIdle
			}
		}
	}

	// Terrain detection — switch to swim/lava animation when standing on
	// water or lava tiles.  Only overrides idle/walk/swim/lava states; sword,
	// dead, etc. take priority.
	if c.AnimState == AnimIdle || c.AnimState == AnimWalk ||
		c.AnimState == AnimSwim || c.AnimState == AnimLava {
		terrain := g.terrainAt(c.X, c.Y, float64(frameW), float64(frameH))
		switch terrain {
		case "lava":
			c.AnimState = AnimLava
		case "water":
			c.AnimState = AnimSwim
		default:
			if c.AnimState == AnimSwim || c.AnimState == AnimLava {
				if c.Moving {
					c.AnimState = AnimWalk
				} else {
					c.AnimState = AnimIdle
				}
			}
		}
	}

	// Clamp to world bounds
	if c.X < 0 {
		c.X = 0
	}
	if c.Y < 0 {
		c.Y = 0
	}
	ww, wh := g.worldSize()
	if c.X > float64(ww-frameW) {
		c.X = float64(ww - frameW)
	}
	if c.Y > float64(wh-frameH) {
		c.Y = float64(wh - frameH)
	}

	if g.conn != nil && (c.X != g.lastSentX || c.Y != g.lastSentY ||
		c.Dir != g.lastSentDir || c.Moving != g.lastSentMoving ||
		c.Mounted != g.lastSentMounted) {
		g.conn.SendJSON(map[string]interface{}{
			"type":    "move",
			"x":       c.X,
			"y":       c.Y,
			"dir":     c.Dir,
			"moving":  c.Moving,
			"anim":    c.AnimState,
			"mounted": c.Mounted,
		})
		g.lastSentX, g.lastSentY = c.X, c.Y
		g.lastSentDir = c.Dir
		g.lastSentMoving = c.Moving
		g.lastSentMounted = c.Mounted
	}
}

// handleViewedProfileClick handles clicks on the viewed player profile buttons.
// Returns true if the click was consumed.
func (g *Game) handleViewedProfileClick(mx, my int) bool {
	p := g.viewedPlayer
	if p == nil {
		return false
	}
	const (
		pw = 480
		ph = 300
		px = screenW/2 - pw/2
		py = screenH/2 - ph/2
	)

	// Close button area (Esc is also handled, but clicks outside close)
	if mx < px || mx > px+pw || my < py || my > py+ph {
		g.viewedPlayer = nil
		return true
	}

	btnY := py + ph - 36
	// Add Friend button (px+18, btnY, 130, 22)
	if mx >= px+18 && mx < px+148 && my >= btnY && my < btnY+22 {
		isFriend := false
		isPending := false
		for _, f := range g.friends {
			if f.Name == p.Name {
				if f.Status == "accepted" {
					isFriend = true
				} else {
					isPending = true
				}
			}
		}
		if !isFriend && !isPending && g.conn != nil {
			g.conn.SendJSON(map[string]interface{}{"type": "friend_add", "target": p.Name})
		}
		return true
	}

	// Invite to Guild button (px+165, btnY, 130, 22)
	if g.myGuild != nil && mx >= px+165 && mx < px+295 && my >= btnY && my < btnY+22 {
		// For now just show a message (guild invite system can be expanded)
		g.chat.AddMessage("", "[Guild] Invite feature coming soon!", true)
		return true
	}

	return false
}

// ──────────────────────────────────────────────────────────────
// Player click / NPC detection
// ──────────────────────────────────────────────────────────────

func (g *Game) handlePlayerClick() {
	if g.localChar == nil {
		return
	}
	mx, my := ebiten.CursorPosition()
	camX, camY := g.camera()
	wx := float64(mx) + camX
	wy := float64(my) + camY

	const pad = 8.0

	lx, ly := g.localChar.X, g.localChar.Y
	if wx >= lx-pad && wx < lx+float64(frameW)+pad &&
		wy >= ly-pad && wy < ly+float64(frameH)+pad {
		g.profileOpen = true
		g.viewedPlayer = nil
		return
	}

	g.mu.Lock()
	var hit *Character
	for _, p := range g.otherPlayers {
		if wx >= p.X-pad && wx < p.X+float64(frameW)+pad &&
			wy >= p.Y-pad && wy < p.Y+float64(frameH)+pad {
			hit = p
			break
		}
	}
	g.mu.Unlock()

	if hit != nil {
		g.viewedPlayer = hit
		g.profileOpen = false
	} else {
		g.viewedPlayer = nil
	}
}

func (g *Game) nearestNPC() (id string, npcType int) {
	if g.localChar == nil {
		return "", -1
	}
	px := g.localChar.X + float64(frameW)/2
	py := g.localChar.Y + float64(frameH)/2
	const radius = 48.0
	g.mu.Lock()
	defer g.mu.Unlock()
	bestDist := radius * radius
	bestID := ""
	bestType := -1
	for nid, npc := range g.npcs {
		if npc.RiddenBy != "" {
			continue // someone is on it: not a thing you can walk up to
		}
		dx := npc.X + float64(frameW)/2 - px
		dy := npc.Y + float64(frameH)/2 - py
		d2 := dx*dx + dy*dy
		if d2 <= bestDist {
			bestDist = d2
			bestID = nid
			bestType = npc.NPCType
		}
	}
	return bestID, bestType
}

// ──────────────────────────────────────────────────────────────
// Combat helpers
// ──────────────────────────────────────────────────────────────

func (g *Game) checkSwordHit() {
	if g.conn == nil || g.localChar == nil {
		return
	}
	hitbox := g.localChar.SwordHitbox()
	if hitbox.Empty() {
		return
	}

	g.mu.Lock()
	for nid, npc := range g.npcs {
		nr := image.Rect(int(npc.X), int(npc.Y), int(npc.X)+frameW, int(npc.Y)+frameH)
		if hitbox.Overlaps(nr) {
			g.conn.SendJSON(map[string]interface{}{
				"type":   "sword_hit",
				"npc_id": nid,
			})
			break
		}
	}
	for pid, p := range g.otherPlayers {
		pr := image.Rect(int(p.X), int(p.Y), int(p.X)+frameW, int(p.Y)+frameH)
		if hitbox.Overlaps(pr) {
			g.conn.SendJSON(map[string]interface{}{
				"type":      "pvp_hit",
				"target_id": pid,
				"x":         g.localChar.X,
				"y":         g.localChar.Y,
			})
			break
		}
	}
	g.mu.Unlock()
}

func (g *Game) checkGralatPickup() {
	if g.localChar == nil || g.conn == nil {
		return
	}
	px := g.localChar.X + float64(frameW)/2
	py := g.localChar.Y + float64(frameH)/2
	const radius = 18.0

	g.grMu.Lock()
	remaining := g.worldGralats[:0]
	toCollect := ""
	for _, gr := range g.worldGralats {
		dx := gr.X - px
		dy := gr.Y - py
		if toCollect == "" && dx*dx+dy*dy <= radius*radius {
			toCollect = gr.ID
		} else {
			remaining = append(remaining, gr)
		}
	}
	if toCollect != "" {
		g.worldGralats = remaining
	}
	g.grMu.Unlock()

	if toCollect != "" {
		g.conn.SendJSON(map[string]string{
			"type":      "collect_gralat",
			"gralat_id": toCollect,
		})
	}
}

// ──────────────────────────────────────────────────────────────
// Chat / commands
// ──────────────────────────────────────────────────────────────

func (g *Game) handleChatMessage(msg string) {
	lower := strings.ToLower(strings.TrimSpace(msg))

	if lower == "/noclip" {
		g.noclip = !g.noclip
		status := "OFF"
		if g.noclip {
			status = "ON"
		}
		g.chat.AddMessage("", "Noclip "+status, true)
		return
	}

	if lower == "/sit" || lower == ":sit" {
		if g.localChar == nil {
			return
		}
		if g.localChar.AnimState == AnimSit {
			g.localChar.AnimState = AnimIdle
		} else {
			g.localChar.AnimState = AnimSit
		}
		if g.conn != nil {
			g.conn.SendJSON(map[string]interface{}{
				"type": "anim_state",
				"anim": g.localChar.AnimState,
			})
		}
		return
	}

	if lower == "/inv" || lower == "/inventaire" {
		g.inventoryMenu.Toggle()
		return
	}

	if strings.HasPrefix(lower, "/removeitem") {
		// Forwarded to server as chat — server handles "/removeitem <player> <item_id>"
		if g.conn != nil {
			g.conn.SendJSON(map[string]string{"type": "chat", "msg": msg})
		}
		return
	}

	if g.conn == nil {
		return
	}
	g.conn.SendJSON(map[string]string{"type": "chat", "msg": msg})
	if strings.HasPrefix(lower, "/giveitem") || strings.HasPrefix(lower, "/itemlist") {
		return
	}
	if g.localChar != nil {
		g.localChar.SetChatMsg(msg)
		if code := containsEmoji(msg); code != "" {
			if img := emojiImageFor(code); img != nil {
				g.localChar.SetEmoji(img)
			}
		}
	}
}

// ──────────────────────────────────────────────────────────────
// Inventory / item use
// ──────────────────────────────────────────────────────────────

func (g *Game) useInventoryItem(itemID string) {
	if g.conn == nil || g.localChar == nil {
		return
	}
	var anim string
	for _, it := range g.localInventory {
		if it.ID == itemID {
			switch itemID {
			case "juggle":
				anim = AnimClassicJuggle
			case "hattrick":
				anim = AnimHatTrick
			case "pompoms":
				anim = AnimPompoms
			default:
				anim = AnimJuggle
			}
			break
		}
	}
	if anim == "" {
		return
	}
	g.localChar.AnimState = anim
	g.conn.SendJSON(map[string]string{"type": "use_item", "item_id": itemID})
}

// ──────────────────────────────────────────────────────────────
// World items
// ──────────────────────────────────────────────────────────────

func (g *Game) findNearWorldItem() *WorldSpawnItem {
	if g.localChar == nil {
		return nil
	}
	px := g.localChar.X + float64(frameW)/2
	py := g.localChar.Y + float64(frameH)/2
	const radius = 48.0

	g.mu.Lock()
	defer g.mu.Unlock()
	bestDist := radius * radius
	var best *WorldSpawnItem
	for i := range g.worldItems {
		wi := &g.worldItems[i]
		dx := wi.X - px
		dy := wi.Y - py
		d2 := dx*dx + dy*dy
		if d2 <= bestDist {
			bestDist = d2
			best = wi
		}
	}
	return best
}

// isBlockedByWorldItem returns true when the given AABB overlaps any world item hitbox.
func (g *Game) isBlockedByWorldItem(px, py, pw, ph float64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.worldItems {
		wi := &g.worldItems[i]
		// hitbox size: use cached sprite size, fall back to 32x32
		var hw, hh float64 = 16, 16
		if img, ok := g.wiSpriteCache[wi.SpritePath]; ok && img != nil {
			hw = float64(img.Bounds().Dx()) / 2
			hh = float64(img.Bounds().Dy()) / 2
		} else if wi.SpritePath == "" {
			if img, ok := g.wiSpriteCache[defaultWorldItemSprite]; ok && img != nil {
				hw = float64(img.Bounds().Dx()) / 2
				hh = float64(img.Bounds().Dy()) / 2
			}
		}
		itemL := wi.X - hw
		itemR := wi.X + hw
		itemT := wi.Y - hh
		itemB := wi.Y + hh
		if px+pw > itemL && px < itemR && py+ph > itemT && py < itemB {
			return true
		}
	}
	return false
}

const defaultWorldItemSprite = "assets/offline/levels/images/dcvip/dcvip_wobblingfurniture0.gif"

func (g *Game) getWorldItemSprite(path string) *ebiten.Image {
	if path == "" {
		path = defaultWorldItemSprite
	}
	if img, ok := g.wiSpriteCache[path]; ok {
		return img
	}
	img, _, err := ebitenutil.NewImageFromFile(path)
	if err != nil {
		g.wiSpriteCache[path] = nil
		return nil
	}
	g.wiSpriteCache[path] = img
	return img
}

// worldItemAtScreen returns the world item whose sprite bounds contain (mx,my)
// in screen space, or nil if none.
func (g *Game) worldItemAtScreen(mx, my int) *WorldSpawnItem {
	camX, camY := g.camera()
	g.mu.Lock()
	defer g.mu.Unlock()
	// iterate in reverse so topmost-drawn item is picked first
	for i := len(g.worldItems) - 1; i >= 0; i-- {
		wi := &g.worldItems[i]
		sx := wi.X - camX
		sy := wi.Y - camY
		img := g.getWorldItemSprite(wi.SpritePath)
		var hw, hh float64
		if img != nil {
			hw = float64(img.Bounds().Dx()) / 2
			hh = float64(img.Bounds().Dy()) / 2
		} else {
			hw, hh = 16, 16
		}
		if float64(mx) >= sx-hw && float64(mx) <= sx+hw &&
			float64(my) >= sy-hh && float64(my) <= sy+hh {
			return wi
		}
	}
	return nil
}

// worldItemDialogRect returns the bounds of the popup dialog (centred on screen).
func worldItemDialogRect() (x, y, w, h int) {
	w, h = 420, 240
	x = screenW/2 - w/2
	y = screenH/2 - h/2
	return
}

// handleWorldItemDialogClick returns true if the click was consumed by the dialog.
func (g *Game) handleWorldItemDialogClick(mx, my int) bool {
	dx, dy, dw, dh := worldItemDialogRect()
	if mx < dx || mx > dx+dw || my < dy || my > dy+dh {
		return false // click outside → close
	}
	wi := g.worldItemDialog
	if wi == nil {
		return false
	}
	// Buy button area (bottom-centre of dialog)
	if wi.Price > 0 {
		btnW, btnH := 140, 32
		btnX := dx + dw/2 - btnW/2
		btnY := dy + dh - btnH - 16
		if mx >= btnX && mx < btnX+btnW && my >= btnY && my < btnY+btnH {
			if g.conn != nil {
				g.conn.SendJSON(map[string]string{
					"type": "buy_world_item",
					"id":   wi.ID,
				})
			}
			g.worldItemDialog = nil
			return true
		}
	}
	return true // click inside but not on button — keep dialog open
}

// ──────────────────────────────────────────────────────────────
// Utility
// ──────────────────────────────────────────────────────────────

func wordWrap(s string, maxChars int) []string {
	words := strings.Fields(s)
	var lines []string
	current := ""
	for _, w := range words {
		if current == "" {
			current = w
		} else if utf8.RuneCountInString(current)+1+utf8.RuneCountInString(w) <= maxChars {
			current += " " + w
		} else {
			lines = append(lines, current)
			current = w
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}
