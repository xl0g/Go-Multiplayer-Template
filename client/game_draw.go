package main

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/text"
	"golang.org/x/image/font/basicfont"
)

// ──────────────────────────────────────────────────────────────
// Main scene render
// ──────────────────────────────────────────────────────────────

func (g *Game) drawPlaying(screen *ebiten.Image) {
	if g.localChar == nil {
		screen.Fill(color.RGBA{12, 12, 22, 255})
		text.Draw(screen, "Connecting to server...", basicfont.Face7x13, 300, 300, color.White)
		return
	}

	camX, camY := g.camera()

	// ── World layer (drawn to worldBuf, then scaled onto screen) ──
	viewW := int(float64(screenW) / g.zoom)
	viewH := int(float64(screenH) / g.zoom)

	g.worldBuf.Clear()
	g.drawWorld(g.worldBuf, camX, camY)

	src := g.worldBuf.SubImage(image.Rect(0, 0, viewW, viewH)).(*ebiten.Image)
	op := &ebiten.DrawImageOptions{}
	op.GeoM.Scale(g.zoom, g.zoom)
	screen.DrawImage(src, op)

	// ── UI layer (always at native resolution, not zoomed) ──
	g.drawHUD(screen)
	g.chat.Draw(screen)
	g.panelMenu.Draw(screen)

	g.drawShopPrompt(screen, camX, camY)

	if g.signDialog != "" {
		g.drawDialog(screen, g.signDialog, 0)
	}
	if g.npcDialog != "" {
		g.drawDialog(screen, g.npcDialog, g.npcGralatN)
	}
	if g.profileOpen {
		g.drawProfile(screen)
	}
	if g.viewedPlayer != nil {
		g.drawViewedProfile(screen)
	}
	g.inventoryMenu.Draw(screen)
	if g.isAdmin {
		g.adminMenu.Draw(screen)
	}
	if g.worldItemDialog != nil {
		g.drawWorldItemDialog(screen)
	}
	if g.localChar != nil && g.localChar.AnimState == AnimDead {
		g.drawDeadOverlay(screen)
	}
	if g.debugOverlay {
		g.drawDebugOverlay(screen)
	}

	// Social panel (Friends / Guilds / Quests) — drawn over everything except overlays
	if g.socialPanel != nil && g.panelMenu != nil {
		activeSub := g.panelMenu.ActiveSocial()
		if activeSub != "" {
			subTop := g.panelMenu.SocialPanelTop()
			g.socialPanel.Draw(screen, activeSub, subTop)
		}
	}
}

// drawWorld renders all world-space elements into dst using the given camera offset.
// dst is typically g.worldBuf (large enough for max zoom-out).
func (g *Game) drawWorld(dst *ebiten.Image, camX, camY float64) {
	vw := float64(screenW) / g.zoom
	vh := float64(screenH) / g.zoom
	if g.activeGMap != "" {
		g.chunkMgr.Draw(dst, camX, camY, vw, vh)
	} else if g.gameMap != nil {
		g.gameMap.Draw(dst, camX, camY)
	} else {
		dst.Fill(color.RGBA{40, 70, 40, 255})
	}

	g.drawWorldGralats(dst, camX, camY)
	g.drawWorldItems(dst, camX, camY)

	g.mu.Lock()
	for _, n := range g.npcs {
		// A ridden horse is drawn by its rider's mount gani; drawing it here too
		// leaves a duplicate horse sprite under the rider.
		if n.RiddenBy != "" {
			continue
		}
		n.Draw(dst, camX, camY)
	}
	for _, p := range g.otherPlayers {
		p.Draw(dst, camX, camY)
	}
	g.mu.Unlock()

	g.localChar.Draw(dst, camX, camY)

	if g.signDialog == "" && g.npcDialog == "" && !g.profileOpen {
		if g.gameMap != nil {
			g.gameMap.DrawSignPrompts(dst, camX, camY)
		}
		g.drawNPCPrompt(dst, camX, camY)
	}
}

// ──────────────────────────────────────────────────────────────
// Camera
// ──────────────────────────────────────────────────────────────

func (g *Game) camera() (camX, camY float64) {
	cww, cwh := g.worldSize()

	// At zoom level z, the viewport covers (screenW/z) × (screenH/z) world units.
	viewW := float64(screenW) / g.zoom
	viewH := float64(screenH) / g.zoom

	if float64(cww) <= viewW {
		camX = -(viewW - float64(cww)) / 2
	} else {
		camX = g.localChar.X + float64(frameW)/2 - viewW/2
		if camX < 0 {
			camX = 0
		}
		if camX > float64(cww)-viewW {
			camX = float64(cww) - viewW
		}
	}
	if float64(cwh) <= viewH {
		camY = -(viewH - float64(cwh)) / 2
	} else {
		camY = g.localChar.Y + float64(frameH)/2 - viewH/2
		if camY < 0 {
			camY = 0
		}
		if camY > float64(cwh)-viewH {
			camY = float64(cwh) - viewH
		}
	}
	return
}

// ──────────────────────────────────────────────────────────────
// HUD
// ──────────────────────────────────────────────────────────────

func (g *Game) drawHUD(screen *ebiten.Image) {
	// Top-left: player name
	DrawRect(screen, 0, 0, 230, 20, color.RGBA{0, 0, 0, 150})
	DrawText(screen, "Player: "+g.localName, 5, 14, color.RGBA{200, 220, 255, 255})

	// Top-right: player/NPC count
	g.mu.Lock()
	pCount := len(g.otherPlayers) + 1
	nCount := len(g.npcs)
	g.mu.Unlock()
	fps := ebiten.CurrentFPS()
	info := fmt.Sprintf("FPS: %.0f  Players: %d  NPCs: %d", fps, pCount, nCount)
	DrawRect(screen, screenW-len(info)*fontW-12, 0, len(info)*fontW+12, 20, color.RGBA{0, 0, 0, 150})
	DrawText(screen, info, screenW-len(info)*fontW-7, 14, color.RGBA{200, 220, 255, 255})

	// HP circles (top-right, below count bar)
	g.drawHPCircles(screen)

	// Gralat count (top-centre)
	gralatStr := fmt.Sprintf("G: %d", g.localGralats)
	gw := len(gralatStr)*fontW + 28
	gx := screenW/2 - gw/2
	DrawRect(screen, gx, 0, gw, 20, color.RGBA{0, 0, 0, 150})
	spr := gralatSprite(1)
	if spr != nil {
		op := &ebiten.DrawImageOptions{}
		op.GeoM.Scale(0.55, 0.55)
		op.GeoM.Translate(float64(gx+4), 1)
		screen.DrawImage(spr, op)
	}
	DrawText(screen, gralatStr, gx+20, 14, colGold)

	// Bottom-left: position
	if g.localChar != nil {
		posStr := fmt.Sprintf("X:%.0f Y:%.0f", g.localChar.X, g.localChar.Y)
		DrawText(screen, posStr, 5, screenH-8, colTextDim)
	}

	// Connection indicator
	if g.conn == nil || g.conn.IsClosed() {
		DrawRect(screen, screenW/2-60, 0, 120, 20, color.RGBA{180, 0, 0, 200})
		DrawText(screen, "DISCONNECTED", screenW/2-6*fontW, 14, color.RGBA{255, 200, 200, 255})
	}

	// Admin badge
	if g.isAdmin {
		badge := "[ADMIN]  Tab=Menu  /giveitem"
		DrawRect(screen, screenW-len(badge)*fontW-14, screenH-22, len(badge)*fontW+10, 16,
			color.RGBA{120, 40, 0, 180})
		DrawText(screen, badge, screenW-len(badge)*fontW-10, screenH-8, color.RGBA{255, 160, 60, 255})
	}

	// Bottom-centre: inventory hint
	invHint := "[I] Inventory"
	DrawText(screen, invHint, screenW/2-len(invHint)*fontW/2, screenH-8, colTextDim)

	// Minimap: only in debug overlay (F3)
	if g.debugOverlay {
		g.drawMinimap(screen)
	}

	// Virtual action buttons
	g.drawVirtualButtons(screen)
}

// drawMinimap renders a small overview of the current map with player positions.
func (g *Game) drawMinimap(screen *ebiten.Image) {
	const (
		mmW  = 100
		mmH  = 100
		mmX  = screenW - mmW - 8
		mmY  = screenH - mmH - 80 // above virtual buttons
		mmBorder = 2
	)

	// Background
	DrawRect(screen, mmX-mmBorder, mmY-mmBorder, mmW+mmBorder*2, mmH+mmBorder*2,
		color.RGBA{60, 80, 120, 200})
	DrawRect(screen, mmX, mmY, mmW, mmH, color.RGBA{8, 14, 28, 220})

	// Label
	DrawText(screen, "MAP", mmX+mmW/2-fontW*3/2, mmY-3, color.RGBA{150, 170, 210, 200})

	ww, wh := g.worldSize()
	if ww <= 0 || wh <= 0 || g.localChar == nil {
		return
	}

	scaleX := float64(mmW) / float64(ww)
	scaleY := float64(mmH) / float64(wh)

	// Other players (blue dots)
	g.mu.Lock()
	for _, p := range g.otherPlayers {
		dx := int(p.X*scaleX) + mmX
		dy := int(p.Y*scaleY) + mmY
		if dx >= mmX && dx < mmX+mmW && dy >= mmY && dy < mmY+mmH {
			DrawRect(screen, dx-1, dy-1, 3, 3, color.RGBA{80, 140, 255, 240})
		}
	}
	// NPCs
	for _, n := range g.npcs {
		dx := int(n.X*scaleX) + mmX
		dy := int(n.Y*scaleY) + mmY
		if dx >= mmX && dx < mmX+mmW && dy >= mmY && dy < mmY+mmH {
			clr := color.RGBA{100, 200, 100, 200}
			if n.NPCType == NPCTypeAggressive {
				clr = color.RGBA{220, 60, 60, 220}
			} else if n.NPCType == NPCTypePassive {
				clr = color.RGBA{200, 220, 100, 200}
			}
			DrawRect(screen, dx, dy, 2, 2, clr)
		}
	}
	g.mu.Unlock()

	// Local player (white dot, always on top)
	lx := int(g.localChar.X*scaleX) + mmX
	ly := int(g.localChar.Y*scaleY) + mmY
	DrawRect(screen, lx-2, ly-2, 5, 5, color.RGBA{255, 255, 255, 255})
	DrawRect(screen, lx-1, ly-1, 3, 3, color.RGBA{255, 210, 50, 255})
}

// drawVirtualButtons renders the grab (glove) and sword virtual buttons.
func (g *Game) drawVirtualButtons(screen *ebiten.Image) {
	// Grab button
	gx, gy, gw, gh := g.virtualGrabRect()
	grabActive := g.localChar != nil && g.localChar.AnimState == AnimGrab
	g.drawVirtualBtn(screen, g.grabKeyImg, gx, gy, gw, gh, grabActive)

	// Sword button
	sx, sy, sw, sh := g.virtualSwordRect()
	swordActive := g.localChar != nil && g.localChar.AnimState == AnimSword
	g.drawVirtualBtn(screen, g.swordKeyImg, sx, sy, sw, sh, swordActive)
}

func (g *Game) drawVirtualBtn(screen *ebiten.Image, img *ebiten.Image, x, y, w, h int, active bool) {
	bgCol := color.RGBA{15, 25, 55, 170}
	borderCol := color.RGBA{70, 90, 160, 200}
	if active {
		bgCol = color.RGBA{50, 80, 200, 220}
		borderCol = color.RGBA{130, 170, 255, 255}
	}
	DrawRect(screen, x, y, w, h, bgCol)
	// Border
	DrawRect(screen, x, y, w, 1, borderCol)
	DrawRect(screen, x, y+h-1, w, 1, borderCol)
	DrawRect(screen, x, y, 1, h, borderCol)
	DrawRect(screen, x+w-1, y, 1, h, borderCol)

	if img != nil {
		iw := img.Bounds().Dx()
		ih := img.Bounds().Dy()
		m := iw
		if ih > m {
			m = ih
		}
		scale := float64(w-8) / float64(m)
		op := &ebiten.DrawImageOptions{}
		op.GeoM.Scale(scale, scale)
		op.GeoM.Translate(
			float64(x)+float64(w)/2-float64(iw)*scale/2,
			float64(y)+float64(h)/2-float64(ih)*scale/2,
		)
		alpha := float32(0.75)
		if active {
			alpha = 1.0
		}
		op.ColorScale.ScaleAlpha(alpha)
		screen.DrawImage(img, op)
	}
}

// ──────────────────────────────────────────────────────────────
// HP circles
// ──────────────────────────────────────────────────────────────

const hpCircleR = 10

func makeHPCircles() [3]*ebiten.Image {
	size := hpCircleR*2 + 2
	var imgs [3]*ebiten.Image
	for k := 0; k < 3; k++ {
		img := ebiten.NewImage(size, size)
		for py := 0; py < size; py++ {
			for px := 0; px < size; px++ {
				dx := float64(px) - float64(hpCircleR)
				dy := float64(py) - float64(hpCircleR)
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist > float64(hpCircleR) {
					continue
				}
				if dist > float64(hpCircleR)-1.5 {
					img.Set(px, py, color.RGBA{20, 20, 20, 220})
					continue
				}
				switch k {
				case 0: // full
					img.Set(px, py, color.RGBA{220, 40, 40, 255})
				case 1: // half
					if px < size/2 {
						img.Set(px, py, color.RGBA{220, 140, 30, 255})
					} else {
						img.Set(px, py, color.RGBA{50, 50, 60, 220})
					}
				case 2: // empty
					img.Set(px, py, color.RGBA{50, 50, 60, 220})
				}
			}
		}
		imgs[k] = img
	}
	return imgs
}

func (g *Game) drawHPCircles(screen *ebiten.Image) {
	if g.hpCircles[0] == nil {
		return
	}
	const gap = 4
	size := hpCircleR*2 + 2
	numCircles := g.localMaxHP / 2
	totalW := numCircles*(size+gap) - gap
	startX := screenW - totalW - 8
	startY := 26

	hp := g.localHP
	for i := 0; i < numCircles; i++ {
		circleHP := hp - i*2
		var idx int
		switch {
		case circleHP >= 2:
			idx = 0
		case circleHP == 1:
			idx = 1
		default:
			idx = 2
		}
		op := &ebiten.DrawImageOptions{}
		op.GeoM.Translate(float64(startX+i*(size+gap)), float64(startY))
		screen.DrawImage(g.hpCircles[idx], op)
	}
}

// ──────────────────────────────────────────────────────────────
// Overlays
// ──────────────────────────────────────────────────────────────

func (g *Game) drawDeadOverlay(screen *ebiten.Image) {
	DrawRect(screen, 0, 0, screenW, screenH, color.RGBA{0, 0, 0, 140})
	msg := "DEAD"
	x := screenW/2 - BigTextW(msg)/2
	DrawBigText(screen, msg, x+2, screenH/2+2, color.RGBA{180, 0, 0, 255})
	DrawBigText(screen, msg, x, screenH/2, color.RGBA{255, 60, 60, 255})
	hint := "Press any key to respawn"
	DrawText(screen, hint, screenW/2-len(hint)*fontW/2, screenH/2+30, color.RGBA{200, 200, 200, 200})
}

func (g *Game) drawDialog(screen *ebiten.Image, msg string, gralatN int) {
	const (
		px = 30
		py = screenH - 130
		pw = screenW - 60
		ph = 110
	)
	DrawPanel(screen, px, py, pw, ph)

	maxChars := (pw - 24) / fontW
	lines := wordWrap(msg, maxChars)
	for i, line := range lines {
		if i >= 4 {
			break
		}
		DrawText(screen, line, px+12, py+22+i*(fontH+4), colTextWhite)
	}

	if gralatN > 0 {
		reward := fmt.Sprintf("+%d gralat(s)!", gralatN)
		DrawText(screen, reward, px+12, py+ph-22, colGold)
	}

	hint := "[F] or [Esc] to close"
	DrawText(screen, hint, px+pw-len(hint)*fontW-10, py+ph-8, colTextDim)
}

// ──────────────────────────────────────────────────────────────
// Profiles
// ──────────────────────────────────────────────────────────────

func (g *Game) drawProfile(screen *ebiten.Image) {
	const (
		pw = 480
		ph = 300
		px = screenW/2 - pw/2
		py = screenH/2 - ph/2
	)
	DrawPanel(screen, px, py, pw, ph)

	title := "MY PROFILE"
	DrawBigText(screen, title, px+(pw-BigTextW(title))/2+2, py+16, colGoldDim)
	DrawBigText(screen, title, px+(pw-BigTextW(title))/2, py+14, colGold)
	DrawHDivider(screen, px+10, py+44, pw-20)

	infoX := px + 18
	row := py + 66

	DrawText(screen, "Name", infoX, row, colGoldDim)
	DrawText(screen, g.localName, infoX, row+fontH+3, colTextWhite)
	row += fontH*2 + 14

	DrawText(screen, "Fortune", infoX, row, colGoldDim)
	row += fontH + 3
	spr := gralatSprite(1)
	if spr != nil {
		op := &ebiten.DrawImageOptions{}
		op.GeoM.Scale(0.75, 0.75)
		op.GeoM.Translate(float64(infoX), float64(row-fontH+1))
		screen.DrawImage(spr, op)
	}
	DrawText(screen, fmt.Sprintf("%d gralats", g.localGralats), infoX+26, row, colGold)
	row += fontH + 14

	totalPlaytime := g.localPlaytime + int(time.Since(g.sessionStart).Seconds())
	DrawText(screen, "Playtime", infoX, row, colGoldDim)
	DrawText(screen, formatPlaytime(totalPlaytime), infoX, row+fontH+3, colTextWhite)
	row += fontH*2 + 14

	DrawText(screen, "Status", infoX, row, colGoldDim)
	DrawRect(screen, infoX, row+fontH+3, 8, 8, color.RGBA{40, 220, 80, 255})
	DrawText(screen, "Online", infoX+12, row+fontH+11, colTextOK)

	DrawRect(screen, px+248, py+50, 1, ph-70, colBorderMid)
	DrawRect(screen, px+249, py+50, 1, ph-70, colBorderHL)

	if g.localChar != nil {
		previewScale := 2.5
		previewW := int(96 * previewScale)
		previewH := int(96 * previewScale)
		previewAreaW := pw - 260
		previewAreaH := ph - 70
		previewX := float64(px+258) + float64(previewAreaW-previewW)/2
		previewY := float64(py+54) + float64(previewAreaH-previewH)/2
		g.localChar.DrawPreview(screen, g.previewImg, previewX, previewY, previewScale)
	}

	hint := "[P] or [Esc] Close"
	DrawText(screen, hint, px+(pw-len(hint)*fontW)/2, py+ph-10, colTextDim)
}

func (g *Game) drawViewedProfile(screen *ebiten.Image) {
	p := g.viewedPlayer
	if p == nil {
		return
	}
	const (
		pw = 480
		ph = 300
		px = screenW/2 - pw/2
		py = screenH/2 - ph/2
	)
	DrawPanel(screen, px, py, pw, ph)

	title := "PLAYER PROFILE"
	DrawBigText(screen, title, px+(pw-BigTextW(title))/2+2, py+16, colGoldDim)
	DrawBigText(screen, title, px+(pw-BigTextW(title))/2, py+14, colGold)
	DrawHDivider(screen, px+10, py+44, pw-20)

	infoX := px + 18
	row := py + 66

	DrawText(screen, "Name", infoX, row, colGoldDim)
	DrawText(screen, p.Name, infoX, row+fontH+3, colTextWhite)
	row += fontH*2 + 14

	DrawText(screen, "Fortune", infoX, row, colGoldDim)
	row += fontH + 3
	spr := gralatSprite(1)
	if spr != nil {
		op := &ebiten.DrawImageOptions{}
		op.GeoM.Scale(0.75, 0.75)
		op.GeoM.Translate(float64(infoX), float64(row-fontH+1))
		screen.DrawImage(spr, op)
	}
	DrawText(screen, fmt.Sprintf("%d gralats", p.Gralats), infoX+26, row, colGold)
	row += fontH + 14

	DrawText(screen, "Playtime", infoX, row, colGoldDim)
	DrawText(screen, formatPlaytime(p.Playtime), infoX, row+fontH+3, colTextWhite)
	row += fontH*2 + 14

	DrawText(screen, "Status", infoX, row, colGoldDim)
	DrawRect(screen, infoX, row+fontH+3, 8, 8, color.RGBA{40, 220, 80, 255})
	DrawText(screen, "Online", infoX+12, row+fontH+11, colTextOK)

	DrawRect(screen, px+248, py+50, 1, ph-70, colBorderMid)
	DrawRect(screen, px+249, py+50, 1, ph-70, colBorderHL)

	previewScale := 2.5
	previewW := int(96 * previewScale)
	previewH := int(96 * previewScale)
	previewAreaW := pw - 260
	previewAreaH := ph - 70
	previewX := float64(px+258) + float64(previewAreaW-previewW)/2
	previewY := float64(py+54) + float64(previewAreaH-previewH)/2
	p.DrawPreview(screen, g.previewImg, previewX, previewY, previewScale)

	// Add Friend button
	isFriend := false
	for _, f := range g.friends {
		if f.Name == p.Name && f.Status == "accepted" {
			isFriend = true
			break
		}
	}
	isPending := false
	for _, f := range g.friends {
		if f.Name == p.Name && f.Status == "pending" {
			isPending = true
			break
		}
	}
	btnY := py + ph - 36
	if !isFriend && !isPending {
		DrawRect(screen, px+18, btnY, 130, 22, color.RGBA{55, 95, 175, 230})
		DrawText(screen, "+ Add Friend", px+24, btnY+15, colTextWhite)
		// Click detection is done in handlePlayerClick
	} else if isPending {
		DrawText(screen, "Request sent", px+18, btnY+15, colTextDim)
	} else {
		DrawText(screen, "♥ Friend", px+18, btnY+15, colTextOK)
	}

	// Invite to guild button
	if g.myGuild != nil {
		DrawRect(screen, px+165, btnY, 130, 22, color.RGBA{95, 55, 175, 230})
		DrawText(screen, "+ Invite to Guild", px+168, btnY+15, colTextWhite)
	}

	hint := "[Esc] Close"
	DrawText(screen, hint, px+(pw-len(hint)*fontW)/2, py+ph-10, colTextDim)
}

// ──────────────────────────────────────────────────────────────
// World entities
// ──────────────────────────────────────────────────────────────

func (g *Game) drawWorldGralats(screen *ebiten.Image, camX, camY float64) {
	g.grMu.Lock()
	gralats := make([]GralatPickup, len(g.worldGralats))
	copy(gralats, g.worldGralats)
	g.grMu.Unlock()

	for _, gr := range gralats {
		sx := gr.X - camX
		sy := gr.Y - camY
		if sx < -32 || sx > float64(screenW)+32 || sy < -32 || sy > float64(screenH)+32 {
			continue
		}
		bob := math.Sin(float64(time.Now().UnixMilli())/400.0+gr.X/60.0) * 3.0
		sy += bob

		spr := gralatSprite(gr.Value)
		if spr != nil {
			op := &ebiten.DrawImageOptions{}
			op.GeoM.Translate(sx-float64(spr.Bounds().Dx())/2, sy-float64(spr.Bounds().Dy())/2)
			screen.DrawImage(spr, op)
		} else {
			DrawRect(screen, int(sx)-6, int(sy)-6, 12, 12, colGold)
		}
	}
}

func (g *Game) drawWorldItems(screen *ebiten.Image, camX, camY float64) {
	g.mu.Lock()
	items := make([]WorldSpawnItem, len(g.worldItems))
	copy(items, g.worldItems)
	g.mu.Unlock()

	// Player centre for proximity checks
	var px, py float64
	nearRadius := 80.0
	if g.localChar != nil {
		px = g.localChar.X + float64(frameW)/2
		py = g.localChar.Y + float64(frameH)/2
	} else {
		nearRadius = 0 // never show if no local char
	}

	// Bobbing offset: small sine wave (~3px, 1.2 Hz)
	ms := float64(time.Now().UnixMilli())
	bob := math.Sin(ms/1000.0*2*math.Pi*1.2) * 3.0

	for _, wi := range items {
		sx := wi.X - camX
		sy := wi.Y - camY
		if sx < -64 || sx > float64(screenW)+64 || sy < -64 || sy > float64(screenH)+64 {
			continue
		}

		img := g.getWorldItemSprite(wi.SpritePath)
		var sprH float64
		if img != nil {
			iw, ih := float64(img.Bounds().Dx()), float64(img.Bounds().Dy())
			sprH = ih
			op := &ebiten.DrawImageOptions{}
			op.GeoM.Translate(sx-iw/2, sy-ih/2)
			screen.DrawImage(img, op)
		} else {
			const s = 12
			sprH = s
			DrawRect(screen, int(sx)-s/2, int(sy)-s/2, s, s, color.RGBA{255, 215, 0, 200})
		}

		label := wi.Name
		lx := int(sx) - len(label)*fontW/2
		DrawText(screen, label, lx, int(sy)-int(sprH/2)-6, colGold)

		// Tap-to-buy icon: only for priced items when player is close enough
		if wi.Price > 0 && g.tapToBuyImg != nil {
			dx := wi.X - px
			dy := wi.Y - py
			if dx*dx+dy*dy <= nearRadius*nearRadius {
				tw := float64(g.tapToBuyImg.Bounds().Dx())
				th := float64(g.tapToBuyImg.Bounds().Dy())
				iconY := sy - sprH/2 - th - 10 + bob
				op := &ebiten.DrawImageOptions{}
				op.GeoM.Translate(sx-tw/2, iconY)
				screen.DrawImage(g.tapToBuyImg, op)
			}
		}
	}
}

func (g *Game) drawShopPrompt(screen *ebiten.Image, camX, camY float64) {
	wi := g.nearWorldItem
	if wi == nil || wi.Price <= 0 {
		return
	}
	sx := wi.X - camX
	sy := wi.Y - camY
	lbl := fmt.Sprintf("[F] Buy %s (%d G)", wi.Name, wi.Price)
	bw := len(lbl)*fontW + 12
	bx := int(sx) - bw/2
	DrawRect(screen, bx, int(sy)-44, bw, 16, color.RGBA{0, 0, 0, 160})
	DrawText(screen, lbl, bx+6, int(sy)-30, colGold)
}

func (g *Game) drawWorldItemDialog(screen *ebiten.Image) {
	wi := g.worldItemDialog
	if wi == nil {
		return
	}
	dx, dy, dw, dh := worldItemDialogRect()

	// Semi-transparent backdrop
	DrawRect(screen, 0, 0, screenW, screenH, color.RGBA{0, 0, 0, 120})

	// Panel background
	DrawPanel(screen, dx, dy, dw, dh)

	// Title bar
	DrawRect(screen, dx, dy, dw, 36, color.RGBA{35, 60, 130, 240})
	DrawRect(screen, dx, dy, dw, 2, color.RGBA{120, 160, 230, 255})
	DrawBigText(screen, wi.Name, dx+(dw-BigTextW(wi.Name))/2+1, dy+24, colGoldDim)
	DrawBigText(screen, wi.Name, dx+(dw-BigTextW(wi.Name))/2, dy+22, colGold)

	DrawHDivider(screen, dx+10, dy+38, dw-20)

	// ── Sprite preview (left column) ───────────────────────────
	const previewSize = 96
	previewX := dx + 20
	previewY := dy + 50
	// background for preview
	DrawRect(screen, previewX, previewY, previewSize, previewSize, color.RGBA{20, 30, 70, 200})
	img := g.getWorldItemSprite(wi.SpritePath)
	if img != nil {
		iw, ih := float64(img.Bounds().Dx()), float64(img.Bounds().Dy())
		scale := float64(previewSize-8) / max2(iw, ih)
		op := &ebiten.DrawImageOptions{}
		op.GeoM.Scale(scale, scale)
		op.GeoM.Translate(
			float64(previewX)+float64(previewSize)/2-iw*scale/2,
			float64(previewY)+float64(previewSize)/2-ih*scale/2,
		)
		screen.DrawImage(img, op)
	}
	// preview border (bright top, dark bottom — 3D feel)
	DrawRect(screen, previewX, previewY, previewSize, 2, color.RGBA{160, 200, 255, 200})
	DrawRect(screen, previewX, previewY+previewSize-2, previewSize, 2, color.RGBA{20, 40, 100, 200})
	DrawRect(screen, previewX, previewY, 2, previewSize, color.RGBA{160, 200, 255, 200})
	DrawRect(screen, previewX+previewSize-2, previewY, 2, previewSize, color.RGBA{20, 40, 100, 200})

	// ── Info panel (right column) ───────────────────────────────
	infoX := previewX + previewSize + 20
	infoY := previewY + 10

	DrawText(screen, "Item", infoX, infoY, colTextDim)
	infoY += fontH + 2
	DrawBigText(screen, wi.Name, infoX, infoY+BigTextH, colTextWhite)
	infoY += BigTextH + 10

	if wi.ItemID != "" {
		DrawText(screen, "ID: "+wi.ItemID, infoX, infoY, colTextDim)
		infoY += fontH + 8
	}

	DrawHDivider(screen, infoX, infoY, dw-(infoX-dx)-16)
	infoY += 10

	if wi.Price > 0 {
		DrawText(screen, "Price", infoX, infoY, colTextDim)
		infoY += fontH + 2
		priceStr := fmt.Sprintf("%d Gralats", wi.Price)
		DrawBigText(screen, priceStr, infoX, infoY+BigTextH, colGold)
	} else {
		DrawBigText(screen, "Free", infoX, infoY+BigTextH, color.RGBA{100, 220, 100, 255})
	}

	// ── Buy / Close button ──────────────────────────────────────
	btnW, btnH := 140, 32
	btnX := dx + dw/2 - btnW/2
	btnY := dy + dh - btnH - 16

	if wi.Price > 0 {
		// green buy button
		DrawRect(screen, btnX, btnY, btnW, btnH, color.RGBA{30, 120, 30, 240})
		DrawRect(screen, btnX, btnY, btnW, 2, color.RGBA{80, 220, 80, 220})
		DrawRect(screen, btnX, btnY+btnH-2, btnW, 2, color.RGBA{10, 60, 10, 200})
		lbl := fmt.Sprintf("Buy  %d G", wi.Price)
		DrawBigText(screen, lbl, btnX+(btnW-BigTextW(lbl))/2, btnY+btnH-7, colTextWhite)
	} else {
		// neutral close button
		DrawRect(screen, btnX, btnY, btnW, btnH, color.RGBA{40, 60, 130, 240})
		DrawRect(screen, btnX, btnY, btnW, 2, color.RGBA{100, 140, 220, 220})
		DrawRect(screen, btnX, btnY+btnH-2, btnW, 2, color.RGBA{15, 25, 70, 200})
		DrawBigText(screen, "Close", btnX+(btnW-BigTextW("Close"))/2, btnY+btnH-7, colTextWhite)
	}

	DrawText(screen, "[Esc] close", dx+(dw-11*fontW)/2, dy+dh-4, colTextDim)
}

func max2(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// ──────────────────────────────────────────────────────────────
// NPC prompt
// ──────────────────────────────────────────────────────────────

func (g *Game) drawNPCPrompt(screen *ebiten.Image, camX, camY float64) {
	if g.localChar != nil && g.localChar.Mounted {
		lbl := "[R] Dismount"
		bw := len(lbl)*fontW + 12
		bx := screenW/2 - bw/2
		DrawRect(screen, bx, screenH-44, bw, 16, color.RGBA{0, 0, 0, 160})
		DrawText(screen, lbl, bx+6, screenH-30, colGold)
		return
	}

	if g.nearNPCID == "" {
		return
	}
	g.mu.Lock()
	npc, ok := g.npcs[g.nearNPCID]
	if !ok {
		g.mu.Unlock()
		return
	}
	sx := npc.X - camX
	sy := npc.Y - camY
	npcType := npc.NPCType
	g.mu.Unlock()

	var lbl string
	if npcType == NPCTypeHorse {
		lbl = "[R] Mount"
	} else {
		lbl = "[F] Talk"
	}
	DrawText(screen, lbl,
		int(sx)+frameW/2-len(lbl)*fontW/2,
		int(sy)-30, colGold)
}

// ──────────────────────────────────────────────────────────────
// Utility
// ──────────────────────────────────────────────────────────────

// nwToTmxName converts a bare NW map name (e.g. "foo.nw") to its TMX
// equivalent ("foo.tmx") for display purposes.
func nwToTmxName(name string) string {
	if strings.HasSuffix(strings.ToLower(name), ".nw") {
		return name[:len(name)-3] + ".tmx"
	}
	return name
}

// ──────────────────────────────────────────────────────────────
// Debug overlay (F3)
// ──────────────────────────────────────────────────────────────

func (g *Game) drawDebugOverlay(screen *ebiten.Image) {
	if g.localChar == nil {
		return
	}
	camX, camY := g.camera()
	vw := float64(screenW) / g.zoom
	vh := float64(screenH) / g.zoom

	// ── Chunk rectangles ──────────────────────────────────────
	if g.activeGMap != "" {
		g.chunkMgr.mu.Lock()
		for _, ch := range g.chunkMgr.chunks {
			// world-space corners → screen-space (account for zoom)
			sx := int((ch.OriginX-camX)*g.zoom)
			sy := int((ch.OriginY-camY)*g.zoom)
			sw := int(chunkPixelW * g.zoom)
			sh := int(chunkPixelH * g.zoom)
			if sx+sw < 0 || sx > screenW || sy+sh < 0 || sy > screenH {
				continue
			}
			// green border
			DrawRect(screen, sx, sy, sw, 2, color.RGBA{0, 255, 80, 180})
			DrawRect(screen, sx, sy+sh-2, sw, 2, color.RGBA{0, 255, 80, 180})
			DrawRect(screen, sx, sy, 2, sh, color.RGBA{0, 255, 80, 180})
			DrawRect(screen, sx+sw-2, sy, 2, sh, color.RGBA{0, 255, 80, 180})
			// chunk label
			lbl := fmt.Sprintf("%d,%d", ch.GridCol, ch.GridRow)
			DrawText(screen, lbl, sx+4, sy+fontH+2, color.RGBA{0, 255, 80, 220})
		}
		// loading chunks (red)
		for key := range g.chunkMgr.loading {
			var gc, gr int
			fmt.Sscanf(key, "%d,%d", &gc, &gr)
			sx := int((float64(gc*chunkPixelW)-camX)*g.zoom)
			sy := int((float64(gr*chunkPixelH)-camY)*g.zoom)
			sw := int(chunkPixelW * g.zoom)
			sh := int(chunkPixelH * g.zoom)
			DrawRect(screen, sx, sy, sw, 2, color.RGBA{255, 80, 0, 160})
			DrawRect(screen, sx, sy+sh-2, sw, 2, color.RGBA{255, 80, 0, 160})
			DrawRect(screen, sx, sy, 2, sh, color.RGBA{255, 80, 0, 160})
			DrawRect(screen, sx+sw-2, sy, 2, sh, color.RGBA{255, 80, 0, 160})
		}
		g.chunkMgr.mu.Unlock()
	}

	// ── NW warp link zones (orange) ──────────────────────────
	tileW := chunkTileW
	tileH := chunkTileH
	if g.activeGMap != "" {
		g.chunkMgr.mu.Lock()
		for _, ch := range g.chunkMgr.chunks {
			for _, lnk := range ch.links {
				isNeighbour := g.chunkMgr.isPartOfGMapLocked(lnk.DestMap)
				wx := ch.OriginX + float64(lnk.X*tileW)
				wy := ch.OriginY + float64(lnk.Y*tileH)
				ww2 := float64(lnk.W * tileW)
				wh2 := float64(lnk.H * tileH)
				sx := int((wx-camX)*g.zoom)
				sy := int((wy-camY)*g.zoom)
				sw := int(ww2 * g.zoom)
				sh := int(wh2 * g.zoom)
				if sx+sw < 0 || sx > screenW || sy+sh < 0 || sy > screenH {
					continue
				}
				if isNeighbour {
					// grey — neighbour chunk link (handled by chunk streaming)
					DrawRect(screen, sx, sy, sw, 2, color.RGBA{160, 160, 160, 120})
					DrawRect(screen, sx, sy+sh-2, sw, 2, color.RGBA{160, 160, 160, 120})
					DrawRect(screen, sx, sy, 2, sh, color.RGBA{160, 160, 160, 120})
					DrawRect(screen, sx+sw-2, sy, 2, sh, color.RGBA{160, 160, 160, 120})
				} else {
					// orange — building / outside map
					DrawRect(screen, sx, sy, sw, sh, color.RGBA{255, 160, 0, 60})
					DrawRect(screen, sx, sy, sw, 2, color.RGBA{255, 160, 0, 220})
					DrawRect(screen, sx, sy+sh-2, sw, 2, color.RGBA{255, 160, 0, 220})
					DrawRect(screen, sx, sy, 2, sh, color.RGBA{255, 160, 0, 220})
					DrawRect(screen, sx+sw-2, sy, 2, sh, color.RGBA{255, 160, 0, 220})
					DrawText(screen, "NW→"+nwToTmxName(lnk.DestMap), sx+4, sy+fontH+2, color.RGBA{255, 200, 80, 255})
				}
			}
		}
		g.chunkMgr.mu.Unlock()
	} else if g.gameMap != nil {
		links := g.gameMap.Links()
		for _, lnk := range links {
			wx := float64(lnk.X * g.gameMap.TileW)
			wy := float64(lnk.Y * g.gameMap.TileH)
			ww2 := float64(lnk.W * g.gameMap.TileW)
			wh2 := float64(lnk.H * g.gameMap.TileH)
			sx := int((wx-camX)*g.zoom)
			sy := int((wy-camY)*g.zoom)
			sw := int(ww2 * g.zoom)
			sh := int(wh2 * g.zoom)
			DrawRect(screen, sx, sy, sw, sh, color.RGBA{255, 160, 0, 60})
			DrawRect(screen, sx, sy, sw, 2, color.RGBA{255, 160, 0, 220})
			DrawRect(screen, sx, sy+sh-2, sw, 2, color.RGBA{255, 160, 0, 220})
			DrawRect(screen, sx, sy, 2, sh, color.RGBA{255, 160, 0, 220})
			DrawRect(screen, sx+sw-2, sy, 2, sh, color.RGBA{255, 160, 0, 220})
			DrawText(screen, "NW→"+nwToTmxName(lnk.DestMap), sx+4, sy+fontH+2, color.RGBA{255, 200, 80, 255})
		}
	}

	// ── TMX switchmap tiles (yellow) and exitmap tiles (cyan) ──
	if g.gameMap != nil {
		tw := float64(g.gameMap.TileW)
		th := float64(g.gameMap.TileH)
		swTile := int(tw * g.zoom)
		shTile := int(th * g.zoom)
		if swTile < 2 {
			swTile = 2
		}
		if shTile < 2 {
			shTile = 2
		}
		for pos, target := range g.gameMap.SwitchmapTiles() {
			wx := float64(pos[0]) * tw
			wy := float64(pos[1]) * th
			sx := int((wx-camX)*g.zoom)
			sy := int((wy-camY)*g.zoom)
			if sx+swTile < 0 || sx > screenW || sy+shTile < 0 || sy > screenH {
				continue
			}
			DrawRect(screen, sx, sy, swTile, shTile, color.RGBA{255, 255, 0, 50})
			DrawRect(screen, sx, sy, swTile, 2, color.RGBA{255, 255, 0, 220})
			DrawRect(screen, sx, sy+shTile-2, swTile, 2, color.RGBA{255, 255, 0, 220})
			DrawRect(screen, sx, sy, 2, shTile, color.RGBA{255, 255, 0, 220})
			DrawRect(screen, sx+swTile-2, sy, 2, shTile, color.RGBA{255, 255, 0, 220})
			DrawText(screen, "→"+target, sx+2, sy+fontH+1, color.RGBA{255, 255, 80, 220})
		}
		for _, pos := range g.gameMap.ExitTileList() {
			wx := float64(pos[0]) * tw
			wy := float64(pos[1]) * th
			sx := int((wx-camX)*g.zoom)
			sy := int((wy-camY)*g.zoom)
			if sx+swTile < 0 || sx > screenW || sy+shTile < 0 || sy > screenH {
				continue
			}
			DrawRect(screen, sx, sy, swTile, shTile, color.RGBA{0, 255, 255, 50})
			DrawRect(screen, sx, sy, swTile, 2, color.RGBA{0, 255, 255, 200})
			DrawRect(screen, sx, sy+shTile-2, swTile, 2, color.RGBA{0, 255, 255, 200})
			DrawRect(screen, sx, sy, 2, shTile, color.RGBA{0, 255, 255, 200})
			DrawRect(screen, sx+swTile-2, sy, 2, shTile, color.RGBA{0, 255, 255, 200})
		}
	}

	// ── Player hitboxes ──────────────────────────────────────────
	c := g.localChar
	{
		// Collision hitbox (red) — what IsBlocked uses (32×32 body with 2px margin)
		hx := int((c.X-camX)*g.zoom)
		hy := int((c.Y-camY)*g.zoom)
		hw := int(float64(frameW) * g.zoom)
		hh := int(float64(frameH) * g.zoom)
		DrawRect(screen, hx, hy, hw, 2, color.RGBA{255, 60, 60, 220})
		DrawRect(screen, hx, hy+hh-2, hw, 2, color.RGBA{255, 60, 60, 220})
		DrawRect(screen, hx, hy, 2, hh, color.RGBA{255, 60, 60, 220})
		DrawRect(screen, hx+hw-2, hy, 2, hh, color.RGBA{255, 60, 60, 220})

		// Warp detection box (magenta) — 80×80 centred on body
		gx := int((c.X-24-camX)*g.zoom)
		gy := int((c.Y-24-camY)*g.zoom)
		gw := int(80 * g.zoom)
		gh := int(80 * g.zoom)
		DrawRect(screen, gx, gy, gw, 2, color.RGBA{255, 0, 255, 200})
		DrawRect(screen, gx, gy+gh-2, gw, 2, color.RGBA{255, 0, 255, 200})
		DrawRect(screen, gx, gy, 2, gh, color.RGBA{255, 0, 255, 200})
		DrawRect(screen, gx+gw-2, gy, 2, gh, color.RGBA{255, 0, 255, 200})

		// Centre cross (white)
		cx := int((c.X+float64(frameW)/2-camX)*g.zoom)
		cy := int((c.Y+float64(frameH)/2-camY)*g.zoom)
		DrawRect(screen, cx-4, cy-1, 9, 3, color.RGBA{255, 255, 255, 220})
		DrawRect(screen, cx-1, cy-4, 3, 9, color.RGBA{255, 255, 255, 220})
	}

	// ── Info panel ────────────────────────────────────────────
	tileCol := int(c.X+float64(frameW)/2) / chunkTileW
	tileRow := int(c.Y+float64(frameH)/2) / chunkTileH

	// Terrain at player position
	terrain := g.terrainAt(c.X, c.Y, float64(frameW), float64(frameH))
	terrainLabel := "ground"
	switch terrain {
	case "water":
		terrainLabel = "WATER"
	case "lava":
		terrainLabel = "LAVA"
	}

	lines := []string{
		fmt.Sprintf("F3 debug | zoom %.2f×  (scroll=zoom, min %.2f max %.2f)", g.zoom, Cfg.ZoomMin, Cfg.ZoomMax),
		fmt.Sprintf("pos  px=(%.0f,%.0f)  tile=(%d,%d)  terrain=%s  anim=%s", c.X, c.Y, tileCol, tileRow, terrainLabel, c.AnimState),
		fmt.Sprintf("cam  X=%.0f  Y=%.0f  vp=%dx%d", camX, camY, int(vw), int(vh)),
	}

	cooldownStr := "ready"
	if g.mapSwitchCooldown > 0 {
		cooldownStr = fmt.Sprintf("%.2fs", g.mapSwitchCooldown)
	}

	if g.activeGMap != "" {
		g.chunkMgr.mu.Lock()
		pGCol := int(g.localChar.X) / chunkPixelW
		pGRow := int(g.localChar.Y) / chunkPixelH
		loaded := len(g.chunkMgr.chunks)
		loading := len(g.chunkMgr.loading)
		totalLinks := 0
		buildingLinks := 0
		nearWarp := ""
		for _, ch := range g.chunkMgr.chunks {
			totalLinks += len(ch.links)
			for _, lnk := range ch.links {
				if !g.chunkMgr.isPartOfGMapLocked(lnk.DestMap) {
					buildingLinks++
				}
			}
		}
		g.chunkMgr.mu.Unlock()
		if lnk, ok := g.chunkMgr.WarpAt(c.X-24, c.Y-24, 80, 80); ok {
			if !g.chunkMgr.IsPartOfGMap(lnk.DestMap) {
				nearWarp = fmt.Sprintf("  *** NW WARP → %s (%.1f,%.1f) ***", nwToTmxName(lnk.DestMap), lnk.DestX, lnk.DestY)
			}
		}
		warpCheckX := c.X - 24
		warpCheckY := c.Y - 24
		chunksX := int(math.Ceil(vw/float64(chunkPixelW))) + 1
		chunksY := int(math.Ceil(vh/float64(chunkPixelH))) + 1
		lines = append(lines,
			fmt.Sprintf("gmap %s | chunk [%d,%d]", g.activeGMap, pGCol, pGRow),
			fmt.Sprintf("chunks loaded=%d loading=%d  links=%d (buildings=%d grey=neighbour)  radius=%dx%d", loaded, loading, totalLinks, buildingLinks, chunksX, chunksY),
			fmt.Sprintf("switch cooldown=%s  warp_box=(%.0f,%.0f,80,80)", cooldownStr, warpCheckX, warpCheckY),
		)
		if nearWarp != "" {
			lines = append(lines, nearWarp)
		}
	} else if g.gameMap != nil {
		links := g.gameMap.Links()

		// Switchmap AABB check — use full gani visual box (48×48), origin = (c.X-8, c.Y-16)
		gaX := c.X - 24
		gaY := c.Y - 24
		tx1 := int(gaX) / g.gameMap.TileW
		ty1 := int(gaY) / g.gameMap.TileH
		tx2 := int(gaX+79) / g.gameMap.TileW
		ty2 := int(gaY+79) / g.gameMap.TileH
		switchHere := g.gameMap.SwitchmapAt(gaX, gaY, 80, 80)
		exitHere := g.gameMap.OnExitTile(gaX, gaY, 80, 80)

		switchStatus := "none"
		if switchHere != "" {
			switchStatus = "→" + switchHere
		} else if exitHere {
			switchStatus = "EXIT (back to prev map)"
		}

		nwWarp := ""
		if lnk, ok := g.gameMap.WarpLinkAt(c.X, c.Y, float64(frameW), float64(frameH)); ok {
			nwWarp = fmt.Sprintf("  *** NW WARP → %s (%.1f,%.1f) ***", nwToTmxName(lnk.DestMap), lnk.DestX, lnk.DestY)
		}

		lines = append(lines,
			fmt.Sprintf("tmx %s", g.currentMapName),
			fmt.Sprintf("nw_links=%d (orange)  tmx_switchmap=%d (yellow)  exitmap=%d (cyan)",
				len(links), len(g.gameMap.SwitchmapTiles()), len(g.gameMap.ExitTileList())),
			fmt.Sprintf("aabb_tiles=(%d,%d)→(%d,%d)  switchmap_here=%s  cooldown=%s",
				tx1, ty1, tx2, ty2, switchStatus, cooldownStr),
		)
		if nwWarp != "" {
			lines = append(lines, nwWarp)
		}
	}

	ph := len(lines)*(fontH+3) + 8
	DrawRect(screen, 0, screenH-ph-2, 620, ph+4, color.RGBA{0, 0, 0, 180})
	for i, l := range lines {
		DrawText(screen, l, 6, screenH-ph+(i*(fontH+3))+fontH, color.RGBA{80, 255, 120, 255})
	}
}

func formatPlaytime(seconds int) string {
	if seconds <= 0 {
		return "0s"
	}
	h := seconds / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	if h > 0 {
		return fmt.Sprintf("%dh %02dm %02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
