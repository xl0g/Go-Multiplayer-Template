package main

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
)

// AdminMenu is the admin control and debug panel. Open with Tab (admin only).
//
// It is keyboard-driven on purpose: it exists to exercise features and inspect
// server state by hand, so every action is one keypress and nothing depends on
// precise clicking. Requests are queued in Out and drained by game_input.go,
// which owns the connection.
type AdminMenu struct {
	visible bool
	tab     adminTab

	// ── Items tab: world-item spawn form ──
	fieldName   *TextInput
	fieldSprite *TextInput
	fieldItemID *TextInput
	fieldPrice  *TextInput
	spawnBtn    *Button
	worldItems  []WorldSpawnItem

	// ── Player tab ──
	fieldTPX     *TextInput
	fieldTPY     *TextInput
	fieldGralats *TextInput

	// ── NPCs tab ──
	npcs     []NPCDebugInfo
	npcSel   int
	npcTop   int // first visible row
	npcQuery *TextInput

	// ── Server tab ──
	info *ServerDebugInfo

	// refreshTimer drives periodic polling of the server while a live tab is open.
	refreshTimer float64

	// Out is the queue of messages for game_input.go to send.
	Out []interface{}

	// Signals kept for the world-item flow, which needs the player position
	// filled in by game.go.
	SpawnReq *AdminSpawnReq
	RemoveID string

	status string // last action feedback, shown in the footer
}

type adminTab int

const (
	tabServer adminTab = iota
	tabNPCs
	tabPlayer
	tabItems
	adminTabCount
)

func (t adminTab) String() string {
	switch t {
	case tabNPCs:
		return "NPCS"
	case tabPlayer:
		return "PLAYER"
	case tabItems:
		return "ITEMS"
	default:
		return "SERVER"
	}
}

// AdminSpawnReq carries the data for a world-item spawn request.
type AdminSpawnReq struct {
	Name       string
	SpritePath string
	ItemID     string
	Price      int
	X, Y       float64 // filled by game.go with localChar position
}

// behaviourCycle is the order the B key steps through on the NPCs tab.
var behaviourCycle = []string{"wander", "roam", "patrol", "passive", "aggressive", "static"}

const (
	adminPW = 620
	adminPH = 520

	adminNPCRows = 12
	adminRowH    = 20
)

func adminPX() int { return screenW/2 - adminPW/2 }
func adminPY() int { return screenH/2 - adminPH/2 }

func NewAdminMenu() *AdminMenu {
	m := &AdminMenu{}
	mk := func(label string, w int) *TextInput { return NewTextInput(0, 0, w, 22, label, false) }

	m.fieldName = mk("Name:", adminPW-160)
	m.fieldSprite = mk("Sprite:", adminPW-160)
	m.fieldItemID = mk("Item ID:", adminPW-160)
	m.fieldPrice = mk("Price:", 90)
	m.fieldSprite.MaxLen = 128
	m.fieldPrice.Value = "0"
	m.spawnBtn = NewButton(0, 0, 110, 26, "Spawn here")

	m.fieldTPX = mk("X:", 110)
	m.fieldTPY = mk("Y:", 110)
	m.fieldGralats = mk("Amount:", 90)
	m.fieldGralats.Value = "100"

	m.npcQuery = mk("Filter:", 180)
	return m
}

func (m *AdminMenu) IsVisible() bool { return m.visible }
func (m *AdminMenu) Open()           { m.visible = true; m.requestRefresh() }
func (m *AdminMenu) Close()          { m.visible = false; m.blurAll() }

func (m *AdminMenu) Toggle() {
	if m.visible {
		m.Close()
	} else {
		m.Open()
	}
}

// fields returns every text input on the currently active tab.
func (m *AdminMenu) fields() []*TextInput {
	switch m.tab {
	case tabItems:
		return []*TextInput{m.fieldName, m.fieldSprite, m.fieldItemID, m.fieldPrice}
	case tabPlayer:
		return []*TextInput{m.fieldTPX, m.fieldTPY, m.fieldGralats}
	case tabNPCs:
		return []*TextInput{m.npcQuery}
	default:
		return nil
	}
}

func (m *AdminMenu) allFields() []*TextInput {
	return []*TextInput{
		m.fieldName, m.fieldSprite, m.fieldItemID, m.fieldPrice,
		m.fieldTPX, m.fieldTPY, m.fieldGralats, m.npcQuery,
	}
}

func (m *AdminMenu) blurAll() {
	for _, f := range m.allFields() {
		f.IsFocused = false
	}
}

// HasFocus reports whether a text field is capturing keys, so the game does not
// also act on them.
func (m *AdminMenu) HasFocus() bool {
	if !m.visible {
		return false
	}
	for _, f := range m.allFields() {
		if f.IsFocused {
			return true
		}
	}
	return false
}

func (m *AdminMenu) SetWorldItems(items []WorldSpawnItem) { m.worldItems = items }

// SetDebugInfo stores the server-wide stats from a debug_info message.
func (m *AdminMenu) SetDebugInfo(info *ServerDebugInfo) { m.info = info }

// SetNPCDebug stores the NPC list from an npc_debug_list message, keeping the
// selection on the same NPC where possible.
func (m *AdminMenu) SetNPCDebug(rows []NPCDebugInfo) {
	var selID string
	if filtered := m.filteredNPCs(); m.npcSel < len(filtered) {
		selID = filtered[m.npcSel].ID
	}
	m.npcs = rows
	if selID == "" {
		return
	}
	for i, n := range m.filteredNPCs() {
		if n.ID == selID {
			m.npcSel = i
			return
		}
	}
	m.clampSel()
}

func (m *AdminMenu) queue(v interface{}) { m.Out = append(m.Out, v) }

// requestRefresh asks the server for whatever the active tab displays.
func (m *AdminMenu) requestRefresh() {
	switch m.tab {
	case tabServer:
		m.queue(map[string]string{"type": "admin_debug_info"})
	case tabNPCs:
		m.queue(map[string]string{"type": "admin_npc_list"})
	}
}

// filteredNPCs applies the name/id filter box.
func (m *AdminMenu) filteredNPCs() []NPCDebugInfo {
	q := strings.ToLower(strings.TrimSpace(m.npcQuery.Value))
	if q == "" {
		return m.npcs
	}
	out := make([]NPCDebugInfo, 0, len(m.npcs))
	for _, n := range m.npcs {
		if strings.Contains(strings.ToLower(n.Name), q) ||
			strings.Contains(strings.ToLower(n.ID), q) ||
			strings.Contains(n.Behaviour, q) {
			out = append(out, n)
		}
	}
	return out
}

func (m *AdminMenu) clampSel() {
	n := len(m.filteredNPCs())
	if n == 0 {
		m.npcSel, m.npcTop = 0, 0
		return
	}
	if m.npcSel >= n {
		m.npcSel = n - 1
	}
	if m.npcSel < 0 {
		m.npcSel = 0
	}
	if m.npcSel < m.npcTop {
		m.npcTop = m.npcSel
	}
	if m.npcSel >= m.npcTop+adminNPCRows {
		m.npcTop = m.npcSel - adminNPCRows + 1
	}
	if m.npcTop < 0 {
		m.npcTop = 0
	}
}

// selectedNPC returns the highlighted NPC, if any.
func (m *AdminMenu) selectedNPC() (NPCDebugInfo, bool) {
	f := m.filteredNPCs()
	if m.npcSel < 0 || m.npcSel >= len(f) {
		return NPCDebugInfo{}, false
	}
	return f[m.npcSel], true
}

func (m *AdminMenu) Update() {
	if !m.visible {
		return
	}
	m.reflow()

	// Poll the live tabs about twice a second.
	m.refreshTimer -= 1.0 / 60.0
	if m.refreshTimer <= 0 {
		m.refreshTimer = 0.5
		m.requestRefresh()
	}

	m.handleMouse()

	// Text fields consume keys while focused.
	focused := false
	for _, f := range m.fields() {
		f.Update()
		if f.IsFocused {
			focused = true
		}
	}
	if !focused {
		m.handleKeys()
	}
}

func (m *AdminMenu) handleMouse() {
	if !inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonLeft) {
		return
	}
	mx, my := ebiten.CursorPosition()
	px, py := adminPX(), adminPY()

	// Tab strip
	for t := adminTab(0); t < adminTabCount; t++ {
		tx := px + 12 + int(t)*98
		if mx >= tx && mx < tx+94 && my >= py+46 && my < py+68 {
			m.setTab(t)
			return
		}
	}

	// Focus a field on the active tab.
	for _, f := range m.fields() {
		f.IsFocused = f.ContainsPoint(mx, my)
	}

	switch m.tab {
	case tabItems:
		if m.spawnBtn.IsClicked() {
			m.trySpawnItem()
		}
		listY := py + 300
		for i, wi := range m.worldItems {
			iy := listY + i*adminRowH
			rbx := px + adminPW - 70
			if mx >= rbx && mx < rbx+60 && my >= iy && my < iy+18 {
				m.RemoveID = wi.ID
				m.status = "Removing " + wi.Name
				return
			}
		}
	case tabNPCs:
		// Click a row to select it.
		rowsY := py + 130
		f := m.filteredNPCs()
		for i := 0; i < adminNPCRows && m.npcTop+i < len(f); i++ {
			iy := rowsY + i*adminRowH
			if my >= iy && my < iy+adminRowH && mx >= px+12 && mx < px+adminPW-12 {
				m.npcSel = m.npcTop + i
				m.clampSel()
				return
			}
		}
	}
}

func (m *AdminMenu) setTab(t adminTab) {
	if t == m.tab {
		return
	}
	m.tab = t
	m.blurAll()
	m.refreshTimer = 0 // refresh immediately on the new tab
}

func (m *AdminMenu) handleKeys() {
	// Tab selection: 1..4, or Left/Right.
	for i, k := range []ebiten.Key{ebiten.Key1, ebiten.Key2, ebiten.Key3, ebiten.Key4} {
		if inpututil.IsKeyJustPressed(k) && adminTab(i) < adminTabCount {
			m.setTab(adminTab(i))
			return
		}
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyRight) {
		m.setTab((m.tab + 1) % adminTabCount)
		return
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyLeft) {
		m.setTab((m.tab + adminTabCount - 1) % adminTabCount)
		return
	}

	switch m.tab {
	case tabNPCs:
		m.handleNPCKeys()
	case tabPlayer:
		m.handlePlayerKeys()
	}
}

func (m *AdminMenu) handleNPCKeys() {
	if inpututil.IsKeyJustPressed(ebiten.KeyDown) {
		m.npcSel++
		m.clampSel()
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyUp) {
		m.npcSel--
		m.clampSel()
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyPageDown) {
		m.npcSel += adminNPCRows
		m.clampSel()
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyPageUp) {
		m.npcSel -= adminNPCRows
		m.clampSel()
	}

	sel, ok := m.selectedNPC()
	if !ok {
		return
	}
	action := func(a string) {
		m.queue(map[string]string{"type": "admin_npc_action", "npc_id": sel.ID, "action": a})
		m.status = sel.Name + " → " + a
	}

	switch {
	case inpututil.IsKeyJustPressed(ebiten.KeyB):
		// Cycle to the next behaviour in the list.
		next := behaviourCycle[0]
		for i, b := range behaviourCycle {
			if b == sel.Behaviour {
				next = behaviourCycle[(i+1)%len(behaviourCycle)]
				break
			}
		}
		m.queue(map[string]string{
			"type": "admin_npc_behaviour", "npc_id": sel.ID, "behaviour": next,
		})
		m.status = sel.Name + " → " + next
	case inpututil.IsKeyJustPressed(ebiten.KeyK):
		action("kill")
	case inpututil.IsKeyJustPressed(ebiten.KeyV):
		action("revive")
	case inpututil.IsKeyJustPressed(ebiten.KeyP):
		action("provoke")
	case inpututil.IsKeyJustPressed(ebiten.KeyG):
		action("bring")
	case inpututil.IsKeyJustPressed(ebiten.KeyH):
		action("home")
	case inpututil.IsKeyJustPressed(ebiten.KeyT):
		// Teleport myself to the NPC.
		m.queue(map[string]interface{}{"type": "admin_teleport", "x": sel.X, "y": sel.Y})
		m.status = "Teleported to " + sel.Name
	}
}

func (m *AdminMenu) handlePlayerKeys() {
	switch {
	case inpututil.IsKeyJustPressed(ebiten.KeyEnter), inpututil.IsKeyJustPressed(ebiten.KeyNumpadEnter):
		m.tryTeleport()
	case inpututil.IsKeyJustPressed(ebiten.KeyS):
		if m.info != nil {
			m.queue(map[string]interface{}{
				"type": "admin_teleport", "x": m.info.SpawnX, "y": m.info.SpawnY,
			})
			m.status = "Teleported to spawn"
		}
	case inpututil.IsKeyJustPressed(ebiten.KeyG):
		m.sendGralats(1)
	case inpututil.IsKeyJustPressed(ebiten.KeyR):
		m.sendGralats(-1)
	case inpututil.IsKeyJustPressed(ebiten.KeyK):
		m.queue(map[string]interface{}{"type": "admin_set_hp", "hp": 0})
		m.status = "HP → 0 (dead)"
	case inpututil.IsKeyJustPressed(ebiten.KeyF):
		m.queue(map[string]interface{}{"type": "admin_set_hp", "hp": 6})
		m.status = "HP → full"
	case inpututil.IsKeyJustPressed(ebiten.KeyE):
		m.queue(map[string]interface{}{"type": "admin_spawn_enemy", "name": "Debug", "count": 1})
		m.status = "Spawned 1 enemy"
	case inpututil.IsKeyJustPressed(ebiten.KeyM):
		m.queue(map[string]interface{}{"type": "admin_spawn_enemy", "name": "Debug", "count": 5})
		m.status = "Spawned 5 enemies"
	}
}

func (m *AdminMenu) sendGralats(sign int) {
	n, err := strconv.Atoi(strings.TrimSpace(m.fieldGralats.Value))
	if err != nil || n == 0 {
		m.status = "Invalid amount"
		return
	}
	m.queue(map[string]interface{}{"type": "admin_gralats", "amount": sign * n})
	m.status = fmt.Sprintf("Gralats %+d", sign*n)
}

func (m *AdminMenu) tryTeleport() {
	x, errX := strconv.ParseFloat(strings.TrimSpace(m.fieldTPX.Value), 64)
	y, errY := strconv.ParseFloat(strings.TrimSpace(m.fieldTPY.Value), 64)
	if errX != nil || errY != nil {
		m.status = "Invalid coordinates"
		return
	}
	m.queue(map[string]interface{}{"type": "admin_teleport", "x": x, "y": y})
	m.status = fmt.Sprintf("Teleported to (%.0f, %.0f)", x, y)
}

func (m *AdminMenu) trySpawnItem() {
	name := strings.TrimSpace(m.fieldName.Value)
	if name == "" {
		m.status = "Name required"
		return
	}
	price, _ := strconv.Atoi(strings.TrimSpace(m.fieldPrice.Value))
	m.SpawnReq = &AdminSpawnReq{
		Name:       name,
		SpritePath: strings.TrimSpace(m.fieldSprite.Value),
		ItemID:     strings.TrimSpace(m.fieldItemID.Value),
		Price:      price,
	}
	m.status = "Spawned " + name
}

// reflow repositions widgets for the active tab.
func (m *AdminMenu) reflow() {
	px, py := adminPX(), adminPY()
	place := func(ti *TextInput, x, y, w int) {
		ti.X, ti.Y, ti.W = x, y, w
	}
	switch m.tab {
	case tabItems:
		place(m.fieldName, px+140, py+90, adminPW-160)
		place(m.fieldSprite, px+140, py+126, adminPW-160)
		place(m.fieldItemID, px+140, py+162, adminPW-160)
		place(m.fieldPrice, px+140, py+198, 90)
		m.spawnBtn.X = px + adminPW/2 - 55
		m.spawnBtn.Y = py + 236
	case tabPlayer:
		place(m.fieldTPX, px+140, py+112, 110)
		place(m.fieldTPY, px+140, py+144, 110)
		place(m.fieldGralats, px+140, py+236, 90)
	case tabNPCs:
		place(m.npcQuery, px+90, py+96, 200)
	}
}

func (m *AdminMenu) Draw(screen *ebiten.Image) {
	if !m.visible {
		return
	}
	m.reflow()
	px, py := adminPX(), adminPY()

	DrawPanel(screen, px, py, adminPW, adminPH)

	title := "ADMIN / DEBUG"
	DrawBigText(screen, title, px+(adminPW-BigTextW(title))/2+2, py+16, colGoldDim)
	DrawBigText(screen, title, px+(adminPW-BigTextW(title))/2, py+14, colGold)

	// ── Tab strip ──
	for t := adminTab(0); t < adminTabCount; t++ {
		tx := px + 12 + int(t)*98
		bg := color.RGBA{40, 40, 52, 220}
		fg := colTextDim
		if t == m.tab {
			bg = color.RGBA{86, 70, 30, 240}
			fg = colGold
		}
		DrawRect(screen, tx, py+46, 94, 22, bg)
		label := fmt.Sprintf("%d %s", int(t)+1, t)
		DrawText(screen, label, tx+(94-len(label)*fontW)/2, py+61, fg)
	}
	DrawHDivider(screen, px+10, py+74, adminPW-20)

	switch m.tab {
	case tabServer:
		m.drawServerTab(screen, px, py)
	case tabNPCs:
		m.drawNPCTab(screen, px, py)
	case tabPlayer:
		m.drawPlayerTab(screen, px, py)
	case tabItems:
		m.drawItemsTab(screen, px, py)
	}

	// ── Footer: status + keys ──
	DrawHDivider(screen, px+10, py+adminPH-30, adminPW-20)
	if m.status != "" {
		DrawText(screen, m.status, px+12, py+adminPH-14, colGold)
	}
	hint := "[1-4/←→] Tab   [Tab/Esc] Close"
	DrawText(screen, hint, px+adminPW-12-len(hint)*fontW, py+adminPH-14, colTextDim)
}

func (m *AdminMenu) drawServerTab(screen *ebiten.Image, px, py int) {
	if m.info == nil {
		DrawText(screen, "Waiting for server…", px+12, py+100, colTextDim)
		return
	}
	i := m.info
	y := py + 92
	row := func(label, value string) {
		DrawText(screen, label, px+12, y, colGoldDim)
		DrawText(screen, value, px+210, y, colTextWhite)
		y += 20
	}

	row("Default map instance", i.DefaultMap)
	row("My map instance", i.MyMap)
	if i.DefaultMap != i.MyMap {
		DrawText(screen, "(different instance — built-in content lives on the default one)",
			px+210, y, color.RGBA{230, 180, 90, 255})
		y += 20
	}
	row("World size", fmt.Sprintf("%.0f × %.0f px", i.WorldW, i.WorldH))
	row("Spawn anchor", fmt.Sprintf("(%.0f, %.0f)", i.SpawnX, i.SpawnY))
	row("Collision loaded", yesNo(i.HasCollision))
	row("View radius", fmt.Sprintf("%.0f px", i.ViewRadius))
	y += 8
	row("Players online", fmt.Sprintf("%d  (%d on my map)", i.Players, i.PlayersOnMyMap))
	row("NPCs", strconv.Itoa(i.NPCs))
	row("Gralats in world", strconv.Itoa(i.Gralats))
	row("World items", strconv.Itoa(i.WorldItems))
	y += 8
	row("Game loop", fmt.Sprintf("%.1f Hz observed", i.ObservedHz))
	row("Uptime", fmtDuration(i.UptimeSec))
	res := "none"
	if len(i.LuaResources) > 0 {
		res = strings.Join(i.LuaResources, ", ")
	}
	row("Lua resources", res)
}

func (m *AdminMenu) drawNPCTab(screen *ebiten.Image, px, py int) {
	m.npcQuery.Draw(screen)

	f := m.filteredNPCs()
	DrawText(screen, fmt.Sprintf("%d NPC(s) on this map", len(f)), px+adminPW-190, py+111, colTextDim)

	// Column headers
	hy := py + 122
	DrawText(screen, "NAME", px+12, hy, colGoldDim)
	DrawText(screen, "BEHAVIOUR", px+190, hy, colGoldDim)
	DrawText(screen, "AI", px+284, hy, colGoldDim)
	DrawText(screen, "HP", px+366, hy, colGoldDim)
	DrawText(screen, "DIST", px+414, hy, colGoldDim)
	DrawText(screen, "BLK", px+470, hy, colGoldDim)

	if len(f) == 0 {
		DrawText(screen, "No NPCs match", px+12, py+150, colTextDim)
		return
	}

	rowsY := py + 130
	for i := 0; i < adminNPCRows && m.npcTop+i < len(f); i++ {
		n := f[m.npcTop+i]
		iy := rowsY + i*adminRowH

		if m.npcTop+i == m.npcSel {
			DrawRect(screen, px+8, iy, adminPW-16, adminRowH, color.RGBA{70, 60, 26, 200})
		}

		nameCol := colTextWhite
		if !n.Alive {
			nameCol = color.RGBA{150, 90, 90, 255}
		}
		DrawText(screen, truncate(n.Name, 21), px+12, iy+14, nameCol)
		DrawText(screen, n.Behaviour, px+190, iy+14, colGold)

		aiCol := colTextDim
		if n.AIState == "chasing" {
			aiCol = color.RGBA{235, 110, 90, 255}
		} else if n.AIState == "returning" {
			aiCol = color.RGBA{120, 180, 230, 255}
		}
		DrawText(screen, n.AIState, px+284, iy+14, aiCol)

		hp := fmt.Sprintf("%d/%d", n.HP, n.MaxHP)
		if n.MaxHP == 0 {
			hp = "immortal"
		}
		DrawText(screen, hp, px+366, iy+14, colTextDim)
		DrawText(screen, fmt.Sprintf("%.0f", n.Dist), px+414, iy+14, colTextDim)

		blkCol := colTextDim
		if n.Blocked > 1.0 {
			blkCol = color.RGBA{235, 150, 80, 255} // visibly stuck
		}
		DrawText(screen, fmt.Sprintf("%.1f", n.Blocked), px+470, iy+14, blkCol)

		if n.MountedBy != "" {
			DrawText(screen, "ridden", px+520, iy+14, colGoldDim)
		} else if n.Waypoints > 0 {
			DrawText(screen, fmt.Sprintf("%dwp", n.Waypoints), px+520, iy+14, colTextDim)
		}
	}

	// Selected NPC detail + key legend
	dy := py + 130 + adminNPCRows*adminRowH + 10
	DrawHDivider(screen, px+10, dy-6, adminPW-20)
	if sel, ok := m.selectedNPC(); ok {
		DrawText(screen, fmt.Sprintf("%s  id=%s  pos=(%.0f,%.0f)  aggro=%q",
			sel.Name, sel.ID, sel.X, sel.Y, sel.Aggro), px+12, dy+10, colTextWhite)
	}
	DrawText(screen, "[↑↓] select  [B] behaviour  [K] kill  [V] revive  [P] provoke",
		px+12, dy+30, colTextDim)
	DrawText(screen, "[G] bring here  [H] send home  [T] teleport to it",
		px+12, dy+46, colTextDim)
}

func (m *AdminMenu) drawPlayerTab(screen *ebiten.Image, px, py int) {
	DrawText(screen, "Teleport", px+12, py+96, colGoldDim)
	m.fieldTPX.Draw(screen)
	m.fieldTPY.Draw(screen)
	DrawText(screen, "[Enter] go to coordinates", px+270, py+128, colTextDim)
	if m.info != nil {
		DrawText(screen, fmt.Sprintf("[S] go to spawn (%.0f, %.0f)", m.info.SpawnX, m.info.SpawnY),
			px+270, py+150, colTextDim)
	}

	DrawHDivider(screen, px+10, py+190, adminPW-20)
	DrawText(screen, "Health", px+12, py+210, colGoldDim)
	DrawText(screen, "[K] kill me    [F] full heal", px+140, py+210, colTextDim)

	DrawHDivider(screen, px+10, py+222, adminPW-20)
	DrawText(screen, "Gralats", px+12, py+240, colGoldDim)
	m.fieldGralats.Draw(screen)
	DrawText(screen, "[G] give    [R] remove", px+250, py+250, colTextDim)

	DrawHDivider(screen, px+10, py+274, adminPW-20)
	DrawText(screen, "Spawn enemies", px+12, py+292, colGoldDim)
	DrawText(screen, "[E] one aggressive enemy    [M] five", px+140, py+292, colTextDim)
	DrawText(screen, "They spawn on your map, near you, and do not respawn on death.",
		px+12, py+314, colTextDim)
}

func (m *AdminMenu) drawItemsTab(screen *ebiten.Image, px, py int) {
	DrawText(screen, "New world item (spawns at your position)", px+12, py+86, colGoldDim)
	for _, f := range []*TextInput{m.fieldName, m.fieldSprite, m.fieldItemID, m.fieldPrice} {
		f.Draw(screen)
	}
	DrawText(screen, "price 0 = decoration, >0 = shop entry", px+250, py+212, colTextDim)
	m.spawnBtn.Draw(screen)

	DrawHDivider(screen, px+10, py+276, adminPW-20)
	DrawText(screen, "World items on this map:", px+12, py+292, colGoldDim)

	listY := py + 300
	maxRows := (adminPH - 340) / adminRowH
	if len(m.worldItems) == 0 {
		DrawText(screen, "none", px+12, listY+14, colTextDim)
		return
	}
	for i, wi := range m.worldItems {
		if i >= maxRows {
			DrawText(screen, "…", px+12, listY+i*adminRowH+14, colTextDim)
			break
		}
		iy := listY + i*adminRowH
		label := wi.Name
		if wi.Price > 0 {
			label = fmt.Sprintf("%s — %dG", wi.Name, wi.Price)
		}
		DrawText(screen, truncate(label, 28), px+12, iy+14, colTextWhite)
		DrawText(screen, fmt.Sprintf("(%.0f,%.0f)", wi.X, wi.Y), px+280, iy+14, colTextDim)

		rbx := px + adminPW - 70
		DrawRect(screen, rbx, iy, 60, 18, color.RGBA{150, 44, 44, 220})
		DrawText(screen, "Remove", rbx+6, iy+14, colTextWhite)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func fmtDuration(sec int) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	if sec < 3600 {
		return fmt.Sprintf("%dm %ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%dh %dm", sec/3600, (sec%3600)/60)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
