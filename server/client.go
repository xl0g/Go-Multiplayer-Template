package main

import (
	"darkzone/MultiTestServer/internal/db"
	"encoding/json"
	"fmt"
	"log"
	mrand "math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const playerMaxHP = 6

// Client represents a single connected player.
type Client struct {
	conn          *websocket.Conn
	send          chan []byte
	hub           *Hub
	userID        int64
	name          string
	playerID      string
	state         PlayerState
	combat        CombatEntity      // shared HP/damage logic — same as NPC
	npcCooldowns  map[string]time.Time
	mountedNPCID  string    // ID of the horse this client is riding, or ""
	sessionStart  time.Time // when the player connected
	savedPlaytime int       // playtime seconds accumulated before this session
	currentMap    string    // which map this client is currently on
	isAdmin       bool      // whether this player has admin privileges
	// lastX/lastY are the position at the previous game-loop tick, used to
	// measure the velocity sent to other clients for dead reckoning.
	lastX, lastY float64
}

// defaultMap is the map instance every entity with an empty mapID belongs to.
// Set from config.json at startup by resolveDefaultMap — it must match the map
// the client actually loads, or built-in content is stranded on a phantom map.
var defaultMap = "maps/GraalRebornMap.tmx"

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// handleWebSocket upgrades the connection, authenticates, and runs read/write loops.
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[WS] Upgrade error:", err)
		return
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var authMsg struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if err := conn.ReadJSON(&authMsg); err != nil || authMsg.Type != "auth" {
		conn.WriteJSON(map[string]string{"type": "auth_error", "msg": "Authentication required"})
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})

	user, err := db.ValidateSession(authMsg.Token)
	if err != nil {
		conn.WriteJSON(map[string]string{"type": "auth_error", "msg": err.Error()})
		conn.Close()
		return
	}

	// Clamp to the actual world bounds, not the 1120px TMX constants — on a
	// larger GMAP world those constants would yank every returning player back
	// into the top-left corner.
	worldW, worldH := globalHub.worldW, globalHub.worldH
	if worldW <= 0 || worldH <= 0 {
		worldW, worldH = mapWidth, mapHeight
	}
	spawnX := clamp(user.LastX, 0, worldW-32)
	spawnY := clamp(user.LastY, 0, worldH-32)
	playerID := fmt.Sprintf("player_%d", user.ID)

	playerCombat := newCombat(playerMaxHP)
	playerCombat.noRespawn = true // players respawn client-side, not by server timer

	client := &Client{
		conn:          conn,
		send:          make(chan []byte, 512),
		hub:           globalHub,
		userID:        user.ID,
		name:          user.Name,
		playerID:      playerID,
		combat:        playerCombat,
		npcCooldowns:  make(map[string]time.Time),
		sessionStart:  time.Now(),
		savedPlaytime: user.Playtime,
		currentMap:    mapOrDefault(user.CurrentMap), // restored: last_x/last_y mean nothing on another map
		isAdmin:       db.IsAdmin(user.ID),
		state: PlayerState{
			ID:       playerID,
			Name:     user.Name,
			X:        spawnX,
			Y:        spawnY,
			Dir:      2,
			Gralats:  user.Gralats,
			Playtime: user.Playtime,
			Body:     user.Body,
			Head:     user.Head,
			Hat:      user.Hat,
			Shield:   user.Shield,
			Sword:    user.Sword,
			HP:       playerMaxHP,
			MaxHP:    playerMaxHP,
		},
	}

	conn.WriteJSON(map[string]interface{}{
		"type":      "auth_ok",
		"id":        playerID,
		"name":      user.Name,
		"x":         spawnX,
		"y":         spawnY,
		"map":       client.currentMap, // instance the x/y above belong to; client loads it
		"gralat_n":  user.Gralats,
		"playtime":  user.Playtime,
		"body":      user.Body,
		"head":      user.Head,
		"hat":       user.Hat,
		"shield":    user.Shield,
		"sword":     user.Sword,
		"is_admin":  client.isAdmin,
	})

	globalHub.register(client)
	// Send current inventory + social data after registration.
	go sendInventoryTo(client)
	go handleFriendList(client)
	go handleGuildInfo(client)
	go handleQuestList(client)
	// Notify Lua resources.
	if globalLuaManager != nil {
		globalLuaManager.TriggerEvent("onPlayerConnect", playerID, user.Name)
	}
	defer func() {
		if globalLuaManager != nil {
			globalLuaManager.TriggerEvent("onPlayerDisconnect", playerID, client.name)
		}
		globalHub.unregister(client)
		conn.Close()
	}()

	// Writer goroutine
	go func() {
		pingTick := time.NewTicker(54 * time.Second)
		defer pingTick.Stop()
		for {
			select {
			case data := <-client.send:
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
					return
				}
			case <-pingTick.C:
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}
	}()

	// Read loop
	conn.SetReadLimit(4096)
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		var base struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &base); err != nil {
			continue
		}

		switch base.Type {
		case "move":
			handleMove(client, raw)
		case "change_map":
			handleChangeMap(client, raw)
		case "cosmetic":
			handleCosmetic(client, raw)
		case "chat":
			handleChat(client, raw)
		case "collect_gralat":
			handleCollectGralat(client, raw)
		case "talk_npc":
			handleTalkNPC(client, raw)
		case "sword_hit":
			handleSwordHit(client, raw)
		case "pvp_hit":
			handlePvPHit(client, raw)
		case "mount_npc":
			handleMountNPC(client, raw)
		case "dismount":
			handleDismount(client, raw)
		case "anim_state":
			handleAnimState(client, raw)
		// ── New handlers ──────────────────────────────────────
		case "use_item":
			handleUseItem(client, raw)
		case "buy_world_item":
			handleBuyWorldItem(client, raw)
		case "admin_spawn_world_item":
			handleAdminSpawnWorldItem(client, raw)
		case "admin_remove_world_item":
			handleAdminRemoveWorldItem(client, raw)
		// ── Admin debug panel ─────────────────────────────────
		case "admin_debug_info":
			handleAdminDebugInfo(client)
		case "admin_npc_list":
			handleAdminNPCList(client)
		case "admin_npc_behaviour":
			handleAdminSetNPCBehaviour(client, raw)
		case "admin_npc_action":
			handleAdminNPCAction(client, raw)
		case "admin_teleport":
			handleAdminTeleport(client, raw)
		case "admin_gralats":
			handleAdminSetGralats(client, raw)
		case "admin_set_hp":
			handleAdminSetHP(client, raw)
		case "admin_spawn_enemy":
			handleAdminSpawnEnemyMsg(client, raw)
		// ── Friends ───────────────────────────────────────────
		case "friend_add":
			handleFriendAdd(client, raw)
		case "friend_accept":
			handleFriendAccept(client, raw)
		case "friend_remove":
			handleFriendRemove(client, raw)
		case "friend_list":
			handleFriendList(client)
		// ── Guilds ────────────────────────────────────────────
		case "guild_create":
			handleGuildCreate(client, raw)
		case "guild_join":
			handleGuildJoin(client, raw)
		case "guild_leave":
			handleGuildLeave(client)
		case "guild_info":
			handleGuildInfo(client)
		case "guild_list":
			handleGuildList(client)
		// ── Quests ────────────────────────────────────────────
		case "quest_list":
			handleQuestList(client)
		case "quest_start":
			handleQuestStart(client, raw)
		}
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func sendJSON(c *Client, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	select {
	case c.send <- data:
	default:
	}
}

// ── Friends handlers ─────────────────────────────────────────────────────────

func handleFriendAdd(c *Client, raw []byte) {
	var msg struct {
		TargetName string `json:"target"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.TargetName == "" {
		return
	}
	targetID, err := db.GetPlayerIDByName(msg.TargetName)
	if err != nil {
		sendJSON(c, map[string]interface{}{
			"type": "friend_result", "success": false, "msg": "Player not found",
		})
		return
	}
	if targetID == c.userID {
		sendJSON(c, map[string]interface{}{
			"type": "friend_result", "success": false, "msg": "You cannot add yourself",
		})
		return
	}
	db.SendFriendRequest(c.userID, targetID)
	sendJSON(c, map[string]interface{}{
		"type": "friend_result", "success": true, "msg": "Friend request sent to " + msg.TargetName,
	})
	// Notify the target if they are online
	globalHub.mu.RLock()
	for other := range globalHub.clients {
		if other.userID == targetID {
			sendJSON(other, map[string]interface{}{
				"type": "friend_request", "from": c.name,
			})
			break
		}
	}
	globalHub.mu.RUnlock()
}

func handleFriendAccept(c *Client, raw []byte) {
	var msg struct {
		FromName string `json:"from"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.FromName == "" {
		return
	}
	fromID, err := db.GetPlayerIDByName(msg.FromName)
	if err != nil {
		return
	}
	db.AcceptFriend(c.userID, fromID)
	// Send updated lists to both parties
	handleFriendList(c)
	globalHub.mu.RLock()
	for other := range globalHub.clients {
		if other.userID == fromID {
			handleFriendList(other)
			break
		}
	}
	globalHub.mu.RUnlock()
}

func handleFriendRemove(c *Client, raw []byte) {
	var msg struct {
		TargetName string `json:"target"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	targetID, err := db.GetPlayerIDByName(msg.TargetName)
	if err != nil {
		return
	}
	db.RemoveFriend(c.userID, targetID)
	handleFriendList(c)
}

func handleFriendList(c *Client) {
	friends := db.GetFriends(c.userID)
	pending := db.GetPendingRequests(c.userID)

	type friendEntry struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Online bool   `json:"online"`
	}

	globalHub.mu.RLock()
	onlineSet := make(map[string]bool)
	for other := range globalHub.clients {
		onlineSet[other.name] = true
	}
	globalHub.mu.RUnlock()

	var list []friendEntry
	for _, f := range friends {
		list = append(list, friendEntry{
			Name:   f.Username,
			Status: f.Status,
			Online: onlineSet[f.Username],
		})
	}
	var reqs []friendEntry
	for _, p := range pending {
		reqs = append(reqs, friendEntry{Name: p.Username, Status: "pending", Online: onlineSet[p.Username]})
	}

	sendJSON(c, map[string]interface{}{
		"type":     "friend_list",
		"friends":  list,
		"requests": reqs,
	})
}

// ── Guild handlers ────────────────────────────────────────────────────────────

func handleGuildCreate(c *Client, raw []byte) {
	var msg struct {
		Name        string `json:"name"`
		Tag         string `json:"tag"`
		Description string `json:"desc"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	guildID, err := db.CreateGuild(msg.Name, msg.Tag, msg.Description, c.userID)
	if err != nil {
		sendJSON(c, map[string]interface{}{"type": "guild_result", "success": false, "msg": err.Error()})
		return
	}
	sendJSON(c, map[string]interface{}{"type": "guild_result", "success": true,
		"msg": "Guild '" + msg.Name + "' created!", "guild_id": guildID})
	handleGuildInfo(c)
}

func handleGuildJoin(c *Client, raw []byte) {
	var msg struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	g, err := db.GetGuildByName(msg.Name)
	if err != nil {
		sendJSON(c, map[string]interface{}{"type": "guild_result", "success": false, "msg": "Guild not found"})
		return
	}
	if err2 := db.JoinGuild(g.ID, c.userID); err2 != nil {
		sendJSON(c, map[string]interface{}{"type": "guild_result", "success": false, "msg": err2.Error()})
		return
	}
	sendJSON(c, map[string]interface{}{"type": "guild_result", "success": true, "msg": "You joined " + g.Name})
	handleGuildInfo(c)
}

func handleGuildLeave(c *Client) {
	if err := db.LeaveGuild(c.userID); err != nil {
		sendJSON(c, map[string]interface{}{"type": "guild_result", "success": false, "msg": err.Error()})
		return
	}
	sendJSON(c, map[string]interface{}{"type": "guild_result", "success": true, "msg": "You left the guild"})
	sendJSON(c, map[string]interface{}{"type": "guild_info", "guild": nil})
}

func handleGuildInfo(c *Client) {
	guildID := db.GetUserGuildID(c.userID)
	if guildID == 0 {
		sendJSON(c, map[string]interface{}{"type": "guild_info", "guild": nil})
		return
	}
	g, err := db.GetGuild(guildID)
	if err != nil {
		sendJSON(c, map[string]interface{}{"type": "guild_info", "guild": nil})
		return
	}
	members := db.GetGuildMembers(guildID)
	type memberEntry struct {
		Name   string `json:"name"`
		Rank   string `json:"rank"`
		Online bool   `json:"online"`
	}
	globalHub.mu.RLock()
	onlineSet := make(map[string]bool)
	for other := range globalHub.clients {
		onlineSet[other.name] = true
	}
	globalHub.mu.RUnlock()

	var mlist []memberEntry
	for _, m := range members {
		mlist = append(mlist, memberEntry{Name: m.Username, Rank: m.Rank, Online: onlineSet[m.Username]})
	}
	sendJSON(c, map[string]interface{}{
		"type": "guild_info",
		"guild": map[string]interface{}{
			"id":      g.ID,
			"name":    g.Name,
			"tag":     g.Tag,
			"leader":  g.LeaderName,
			"desc":    g.Description,
			"members": mlist,
		},
	})
}

func handleGuildList(c *Client) {
	guilds := db.ListGuilds()
	type guildEntry struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		Tag     string `json:"tag"`
		Leader  string `json:"leader"`
		Members int    `json:"members"`
	}
	var list []guildEntry
	for _, g := range guilds {
		list = append(list, guildEntry{g.ID, g.Name, g.Tag, g.LeaderName, g.MemberCount})
	}
	sendJSON(c, map[string]interface{}{"type": "guild_list", "guilds": list})
}

// ── Quest handlers ────────────────────────────────────────────────────────────

func handleQuestList(c *Client) {
	playerQuests := db.GetPlayerQuests(c.userID)
	progressMap := make(map[string]db.PlayerQuestRow)
	for _, pq := range playerQuests {
		progressMap[pq.QuestID] = pq
	}

	type questEntry struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"desc"`
		Objective   string `json:"objective"`
		Progress    int    `json:"progress"`
		Required    int    `json:"required"`
		Completed   bool   `json:"completed"`
		Reward      int    `json:"reward"`
	}

	var list []questEntry
	for _, def := range db.QuestDefs {
		pq := progressMap[def.ID]
		list = append(list, questEntry{
			ID:          def.ID,
			Name:        def.Name,
			Description: def.Description,
			Objective:   def.Objective,
			Progress:    pq.Progress,
			Required:    def.ObjectiveCount,
			Completed:   pq.Completed,
			Reward:      def.RewardGralats,
		})
	}
	sendJSON(c, map[string]interface{}{"type": "quest_list", "quests": list})
}

func handleQuestStart(c *Client, raw []byte) {
	var msg struct {
		QuestID string `json:"quest_id"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	if err := db.StartQuest(c.userID, msg.QuestID); err != nil {
		sendJSON(c, map[string]interface{}{"type": "quest_result", "success": false, "msg": err.Error()})
		return
	}
	sendJSON(c, map[string]interface{}{"type": "quest_result", "success": true, "msg": "Quest started!"})
	handleQuestList(c)
}

// advanceKillQuest is called when the player kills an aggressive NPC.
func advanceKillQuest(c *Client) {
	for _, def := range db.QuestDefs {
		if def.ObjectiveType == "kill_npc" && def.ObjectiveTarget == "aggressive" {
			prog, completed, reward := db.UpdateQuestProgress(c.userID, def.ID, 1)
			if completed {
				sendJSON(c, map[string]interface{}{
					"type": "quest_complete",
					"quest_id": def.ID,
					"name":     def.Name,
					"reward":   reward,
				})
				newTotal := 0
				db.DB().QueryRow(`SELECT gralats FROM users WHERE id=$1`, c.userID).Scan(&newTotal)
				sendJSON(c, map[string]interface{}{"type": "gralat_update", "gralat_n": newTotal})
			} else if prog > 0 {
				sendJSON(c, map[string]interface{}{
					"type":     "quest_update",
					"quest_id": def.ID,
					"progress": prog,
					"required": def.ObjectiveCount,
				})
			}
		}
	}
}

// advanceTalkQuest is called when the player talks to an NPC of a given type.
func advanceTalkQuest(c *Client, npcTypeName string) {
	for _, def := range db.QuestDefs {
		if def.ObjectiveType == "talk_npc" && def.ObjectiveTarget == npcTypeName {
			prog, completed, reward := db.UpdateQuestProgress(c.userID, def.ID, 1)
			if completed {
				sendJSON(c, map[string]interface{}{
					"type": "quest_complete",
					"quest_id": def.ID,
					"name":     def.Name,
					"reward":   reward,
				})
			} else if prog > 0 {
				_ = prog
			}
		}
	}
}

// advanceGralatQuest updates gralat-collection quests.
func advanceGralatQuest(c *Client, totalGralats int) {
	for _, def := range db.QuestDefs {
		if def.ObjectiveType == "collect_gralats" {
			pq := db.GetPlayerQuests(c.userID)
			for _, pqr := range pq {
				if pqr.QuestID == def.ID && pqr.Completed {
					goto next
				}
			}
			if totalGralats >= def.ObjectiveCount {
				_, completed, reward := db.UpdateQuestProgress(c.userID, def.ID, def.ObjectiveCount)
				if completed {
					sendJSON(c, map[string]interface{}{
						"type":     "quest_complete",
						"quest_id": def.ID,
						"name":     def.Name,
						"reward":   reward,
					})
				}
			}
		next:
		}
	}
}

// ──────────────────────────────────────────────────────────────
// Existing handlers
// ──────────────────────────────────────────────────────────────

func handleMove(c *Client, raw []byte) {
	var msg struct {
		X         float64 `json:"x"`
		Y         float64 `json:"y"`
		Dir       int     `json:"dir"`
		Moving    bool    `json:"moving"`
		AnimState string  `json:"anim,omitempty"`
		Mounted   bool    `json:"mounted,omitempty"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	globalHub.mu.Lock()
	c.state.X = clamp(msg.X, 0, globalHub.worldW-32)
	c.state.Y = clamp(msg.Y, 0, globalHub.worldH-32)
	c.state.Dir = msg.Dir
	c.state.Moving = msg.Moving
	c.state.AnimState = msg.AnimState
	c.state.Mounted = msg.Mounted
	// Move the horse the player is riding
	if c.mountedNPCID != "" {
		globalHub.updateHorsePos(c.playerID, c.state.X, c.state.Y)
	}
	globalHub.mu.Unlock()
}

func handleChangeMap(c *Client, raw []byte) {
	var msg struct {
		Map string `json:"map"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.Map == "" {
		return
	}
	globalHub.mu.Lock()
	c.currentMap = msg.Map
	globalHub.mu.Unlock()
}

func handleCosmetic(c *Client, raw []byte) {
	var msg struct {
		Body   string `json:"body"`
		Head   string `json:"head"`
		Hat    string `json:"hat"`
		Shield string `json:"shield"`
		Sword  string `json:"sword"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	globalHub.mu.Lock()
	c.state.Body = msg.Body
	c.state.Head = msg.Head
	c.state.Hat = msg.Hat
	c.state.Shield = msg.Shield
	c.state.Sword = msg.Sword
	globalHub.mu.Unlock()
	db.SaveCosmetics(c.userID, msg.Body, msg.Head, msg.Hat, msg.Shield, msg.Sword)
}

func handleChat(c *Client, raw []byte) {
	var msg struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	txt := strings.TrimSpace(msg.Msg)
	if len(txt) == 0 || len(txt) > 200 {
		return
	}

	// Server-side commands (not broadcast as chat).
	lower := strings.ToLower(txt)
	if strings.HasPrefix(lower, "/giveitem ") {
		handleGiveItemCommand(c, txt)
		return
	}
	if strings.HasPrefix(lower, "/removeitem ") {
		handleRemoveItemCommand(c, txt)
		return
	}
	if lower == "/itemlist" {
		var names []string
		for id := range allItemDefs {
			names = append(names, id)
		}
		sendDirectMsg(c, "Items disponibles: "+strings.Join(names, ", "))
		return
	}
	// /spawnenemy [name] — admin only
	if strings.HasPrefix(lower, "/spawnenemy") {
		handleSpawnEnemyCommand(c, txt)
		return
	}
	// Lua resource management commands (admin only).
	if lower == "/resources" ||
		strings.HasPrefix(lower, "/start ") ||
		strings.HasPrefix(lower, "/stop ") ||
		strings.HasPrefix(lower, "/restart ") {
		handleLuaCommand(c, txt, lower)
		return
	}

	db.SaveChat(c.name, txt)
	globalHub.broadcast(map[string]interface{}{
		"type": "chat",
		"from": c.name,
		"msg":  txt,
	})
	// Notify Lua resources of the chat message.
	if globalLuaManager != nil {
		globalLuaManager.TriggerEvent("onPlayerChat", c.playerID, c.name, txt)
	}
}

// handleSpawnEnemyCommand spawns an aggressive NPC on the player's current map.
// Usage: /spawnenemy [name]   (admin only)
func handleSpawnEnemyCommand(c *Client, txt string) {
	if !c.isAdmin {
		sendDirectMsg(c, "Permission denied.")
		return
	}
	parts := strings.SplitN(txt, " ", 2)
	name := "Ennemi"
	if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
		name = strings.TrimSpace(parts[1])
	}
	mapID := c.currentMap
	if mapID == "" {
		mapID = defaultMap
	}
	globalHub.spawnEnemyAt(name, mapID, c.state.X, c.state.Y)
	sendDirectMsg(c, fmt.Sprintf("[SPAWN] %q spawné à (%.0f, %.0f) sur %s", name, c.state.X, c.state.Y, mapID))
}

// handleLuaCommand processes /start, /stop, /restart, /resources (admin only).
func handleLuaCommand(c *Client, txt, lower string) {
	if !c.isAdmin {
		sendDirectMsg(c, "Permission denied.")
		return
	}
	if globalLuaManager == nil {
		sendDirectMsg(c, "Lua manager not initialized.")
		return
	}

	if lower == "/resources" {
		names := globalLuaManager.List()
		if len(names) == 0 {
			sendDirectMsg(c, "[LUA] No resources running.")
		} else {
			sendDirectMsg(c, "[LUA] Running resources: "+strings.Join(names, ", "))
		}
		return
	}

	parts := strings.Fields(txt)
	if len(parts) < 2 {
		sendDirectMsg(c, "Usage: /start|stop|restart <resource>")
		return
	}
	cmd := strings.ToLower(parts[0])
	name := parts[1]

	var err error
	switch cmd {
	case "/start":
		err = globalLuaManager.Start(name)
	case "/stop":
		err = globalLuaManager.Stop(name)
	case "/restart":
		err = globalLuaManager.Restart(name)
	}

	if err != nil {
		sendDirectMsg(c, "[LUA] Error: "+err.Error())
	} else {
		sendDirectMsg(c, "[LUA] "+cmd+" "+name+" OK")
	}
}

func handleCollectGralat(c *Client, raw []byte) {
	var msg struct {
		GralatID string `json:"gralat_id"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.GralatID == "" {
		return
	}
	value := globalHub.collectGralat(msg.GralatID, mapOrDefault(c.currentMap), c.state.X, c.state.Y)
	if value <= 0 {
		return // already taken, on another map, or out of reach
	}
	newTotal, _ := db.AddGralats(c.userID, value)
	globalHub.mu.Lock()
	c.state.Gralats = newTotal
	globalHub.mu.Unlock()

	data, _ := json.Marshal(map[string]interface{}{
		"type":     "gralat_update",
		"gralat_n": newTotal,
	})
	select {
	case c.send <- data:
	default:
	}
	log.Printf("[GRALAT] %s collecte %s (+%d → %d)", c.name, msg.GralatID, value, newTotal)
	go advanceGralatQuest(c, newTotal)
}

func handleTalkNPC(c *Client, raw []byte) {
	var msg struct {
		NPCID string `json:"npc_id"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.NPCID == "" {
		return
	}

	// Copy every field needed while holding the lock. The 60 Hz game loop mutates
	// NPC state under the write lock, so the *NPC must not be dereferenced once
	// the lock is released.
	// Resolve dialog here too: Lua custom dialog takes priority over the
	// type-based default.
	var (
		found      bool
		npcName    string
		npcType    int
		dialogText string
		gMin, gMax int
	)
	globalHub.mu.RLock()
	if n := globalHub.findNPCOnMap(msg.NPCID, mapOrDefault(c.currentMap)); n != nil {
		found = true
		npcName = n.state.Name
		npcType = n.state.NPCType
		if n.customDialog != "" {
			dialogText = n.customDialog
			gMin, gMax = n.customGMin, n.customGMax
		} else {
			def := npcDialogDefs[npcType%len(npcDialogDefs)]
			dialogText = def.msg
			gMin, gMax = def.minG, def.maxG
		}
	}
	globalHub.mu.RUnlock()
	if !found || npcType == NPCTypeHorse {
		return // not on this map, or horses (which don't talk)
	}

	const cooldown = 120 * time.Second
	if last, ok := c.npcCooldowns[msg.NPCID]; ok && time.Since(last) < cooldown {
		remaining := int((cooldown - time.Since(last)).Seconds())
		data, _ := json.Marshal(map[string]interface{}{
			"type":     "npc_dialog",
			"msg":      fmt.Sprintf("%s: I have nothing more to say for now... (%ds)", npcName, remaining),
			"gralat_n": 0,
		})
		select {
		case c.send <- data:
		default:
		}
		return
	}

	if gMax < gMin {
		gMax = gMin
	}
	gralatN := gMin
	if gMax > gMin {
		gralatN = gMin + mrand.Intn(gMax-gMin+1)
	}

	newTotal, _ := db.AddGralats(c.userID, gralatN)

	globalHub.mu.Lock()
	c.state.Gralats = newTotal
	globalHub.mu.Unlock()

	c.npcCooldowns[msg.NPCID] = time.Now()

	dialog := fmt.Sprintf("%s: %s", npcName, dialogText)
	data, _ := json.Marshal(map[string]interface{}{
		"type":     "npc_dialog",
		"msg":      dialog,
		"gralat_n": gralatN,
	})
	select {
	case c.send <- data:
	default:
	}
	log.Printf("[NPC] %s -> %s: +%d gralats (%d total)", npcName, c.name, gralatN, newTotal)
	switch npcType {
	case NPCTypeMerchant:
		go advanceTalkQuest(c, "merchant")
	case NPCTypeVillager:
		go advanceTalkQuest(c, "villager")
	}
}

// ──────────────────────────────────────────────────────────────
// Combat
// ──────────────────────────────────────────────────────────────

func handleSwordHit(c *Client, raw []byte) {
	var msg struct {
		NPCID string `json:"npc_id"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.NPCID == "" {
		return
	}

	// Server-side proximity validation (within 100 px), scoped to the attacker's map.
	const maxReach = 100.0
	myMap := mapOrDefault(c.currentMap)
	var npcX, npcY float64
	var found bool
	globalHub.mu.RLock()
	if n := globalHub.findNPCOnMap(msg.NPCID, myMap); n != nil && n.combat.IsAlive() {
		npcX, npcY = n.state.X, n.state.Y
		found = true
	}
	globalHub.mu.RUnlock()

	if !found {
		return // dead, missing, or on another map
	}

	dx := npcX - c.state.X
	dy := npcY - c.state.Y
	if dx*dx+dy*dy > maxReach*maxReach {
		return // cheating or network lag — ignore
	}

	newHP, killed := globalHub.damageNPC(msg.NPCID, myMap, 1)
	if newHP < 0 {
		return // NPC on cooldown or immortal
	}

	data, _ := json.Marshal(map[string]interface{}{
		"type":   "npc_damage",
		"npc_id": msg.NPCID,
		"hp":     newHP,
		"killed": killed,
	})
	select {
	case c.send <- data:
	default:
	}

	if killed {
		log.Printf("[COMBAT] %s killed %s", c.name, msg.NPCID)
		// Advance kill quests if the NPC was aggressive
		globalHub.mu.RLock()
		for _, n := range globalHub.npcs {
			if n.state.ID == msg.NPCID && mapOrDefault(n.mapID) == myMap &&
				n.state.NPCType == NPCTypeAggressive {
				go advanceKillQuest(c)
				break
			}
		}
		globalHub.mu.RUnlock()
	} else {
		log.Printf("[COMBAT] %s hit %s → HP %d", c.name, msg.NPCID, newHP)
	}
}

func handlePvPHit(attacker *Client, raw []byte) {
	var msg struct {
		TargetID string `json:"target_id"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.TargetID == "" {
		return
	}

	// Proximity validation (within 120px), and both players must be on the same
	// map — coordinates repeat across map instances, so without this check a
	// player can hit someone standing at the same spot on a different map.
	const maxReach = 120.0
	attackerMap := mapOrDefault(attacker.currentMap)
	var target *Client
	globalHub.mu.RLock()
	for c := range globalHub.clients {
		if c.playerID == msg.TargetID {
			if mapOrDefault(c.currentMap) == attackerMap {
				dx := c.state.X - attacker.state.X
				dy := c.state.Y - attacker.state.Y
				if dx*dx+dy*dy <= maxReach*maxReach {
					target = c
				}
			}
			break
		}
	}
	globalHub.mu.RUnlock()

	if target == nil {
		return
	}

	// Apply damage via shared CombatEntity — same rules as NPC attacks.
	globalHub.mu.Lock()
	newHP, killed := target.combat.Damage(1)
	if newHP >= 0 {
		target.state.HP = newHP
	}
	targetHP := target.state.HP
	if killed {
		target.state.AnimState = "dead"
	}
	globalHub.mu.Unlock()

	if newHP < 0 {
		return // immune (invulnerability window)
	}

	data, _ := json.Marshal(map[string]interface{}{
		"type":   "pvp_damage",
		"from":   attacker.name,
		"damage": 1,
		"atk_x":  attacker.state.X,
		"atk_y":  attacker.state.Y,
		"hp":     targetHP,
	})
	select {
	case target.send <- data:
	default:
	}
	log.Printf("[PVP] %s hit %s → HP %d", attacker.name, target.name, targetHP)
}

// ──────────────────────────────────────────────────────────────
// Mount
// ──────────────────────────────────────────────────────────────

func handleMountNPC(c *Client, raw []byte) {
	var msg struct {
		NPCID string `json:"npc_id"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.NPCID == "" {
		return
	}

	// Proximity check (within 64 px), scoped to the rider's map.
	const maxDist = 64.0
	myMap := mapOrDefault(c.currentMap)
	var npcX, npcY float64
	var found bool
	globalHub.mu.RLock()
	if n := globalHub.findNPCOnMap(msg.NPCID, myMap); n != nil {
		npcX, npcY = n.state.X, n.state.Y
		found = true
	}
	globalHub.mu.RUnlock()

	if !found {
		return // missing or on another map — without this an unknown ID would
		// be compared against (0,0) and pass for players standing near the origin
	}

	dx := npcX - c.state.X
	dy := npcY - c.state.Y
	if dx*dx+dy*dy > maxDist*maxDist {
		return
	}

	if !globalHub.mountNPC(msg.NPCID, myMap, c.playerID) {
		return // horse already taken or wrong type
	}

	c.mountedNPCID = msg.NPCID

	globalHub.mu.Lock()
	c.state.Mounted = true
	c.state.AnimState = "ride"
	globalHub.mu.Unlock()

	data, _ := json.Marshal(map[string]interface{}{
		"type":   "mount_ok",
		"npc_id": msg.NPCID,
	})
	select {
	case c.send <- data:
	default:
	}
	log.Printf("[MOUNT] %s mounted %s", c.name, msg.NPCID)
}

func handleDismount(c *Client, raw []byte) {
	if c.mountedNPCID == "" {
		return
	}
	globalHub.dismountNPC(c.playerID)
	c.mountedNPCID = ""

	globalHub.mu.Lock()
	c.state.Mounted = false
	c.state.AnimState = ""
	globalHub.mu.Unlock()

	data, _ := json.Marshal(map[string]string{"type": "dismount_ok"})
	select {
	case c.send <- data:
	default:
	}
}

func handleAnimState(c *Client, raw []byte) {
	var msg struct {
		Anim    string `json:"anim"`
		Mounted bool   `json:"mounted,omitempty"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	globalHub.mu.Lock()
	if c.state.AnimState == "dead" && msg.Anim != "dead" {
		// Client-side respawn: reset combat state so hits land correctly.
		c.combat = newCombat(playerMaxHP)
		c.combat.noRespawn = true
		c.state.HP = playerMaxHP
	}
	c.state.AnimState = msg.Anim
	globalHub.mu.Unlock()
}

// ──────────────────────────────────────────────────────────────
// Inventory / Item helpers
// ──────────────────────────────────────────────────────────────

// sendDirectMsg sends a system message only to one client.
func sendDirectMsg(c *Client, msg string) {
	data, _ := json.Marshal(map[string]string{"type": "system", "msg": msg})
	select {
	case c.send <- data:
	default:
	}
}

// sendInventoryTo builds and sends the player's current inventory.
func sendInventoryTo(c *Client) {
	rows := db.GetInventory(c.userID)
	items := make([]InventoryItem, 0, len(rows))
	for _, r := range rows {
		if def, ok := allItemDefs[r.ItemID]; ok {
			items = append(items, inventoryItemFromDef(def, r.Quantity))
		}
	}
	data, _ := json.Marshal(map[string]interface{}{
		"type":      "inventory_data",
		"inventory": items,
	})
	select {
	case c.send <- data:
	default:
	}
}

// handleGiveItemCommand processes "/giveitem <player> <item_id>" (admin only).
func handleGiveItemCommand(c *Client, txt string) {
	if !c.isAdmin {
		sendDirectMsg(c, "Permission refusée.")
		return
	}
	parts := strings.Fields(txt)
	if len(parts) < 3 {
		sendDirectMsg(c, "Usage: /giveitem <joueur> <item_id>")
		return
	}
	targetName := parts[1]
	itemID := parts[2]

	def, ok := allItemDefs[itemID]
	if !ok {
		sendDirectMsg(c, "Item inconnu: "+itemID)
		return
	}
	targetUserID, err := db.GetPlayerIDByName(targetName)
	if err != nil {
		sendDirectMsg(c, "Joueur introuvable: "+targetName)
		return
	}
	db.GiveItem(targetUserID, itemID, 1)

	// Refresh inventory for target if online.
	globalHub.mu.RLock()
	for cl := range globalHub.clients {
		if strings.EqualFold(cl.name, targetName) {
			go sendInventoryTo(cl)
			break
		}
	}
	globalHub.mu.RUnlock()

	sendDirectMsg(c, fmt.Sprintf("Item '%s' donné à %s.", def.Name, targetName))
	globalHub.broadcastSystem(fmt.Sprintf("%s a reçu %s !", targetName, def.Name))
	log.Printf("[ADMIN] %s gave '%s' to %s", c.name, itemID, targetName)
}

// handleRemoveItemCommand processes "/removeitem <player> <item_id>" (admin only).
func handleRemoveItemCommand(c *Client, txt string) {
	if !c.isAdmin {
		sendDirectMsg(c, "Permission denied.")
		return
	}
	parts := strings.Fields(txt)
	if len(parts) < 3 {
		sendDirectMsg(c, "Usage: /removeitem <player> <item_id>")
		return
	}
	targetName := parts[1]
	itemID := parts[2]

	if _, ok := allItemDefs[itemID]; !ok {
		sendDirectMsg(c, "Unknown item: "+itemID)
		return
	}
	targetUserID, err := db.GetPlayerIDByName(targetName)
	if err != nil {
		sendDirectMsg(c, "Player not found: "+targetName)
		return
	}
	if !db.RemoveItem(targetUserID, itemID) {
		sendDirectMsg(c, targetName+" does not have item: "+itemID)
		return
	}

	// Refresh inventory for target if online.
	globalHub.mu.RLock()
	for cl := range globalHub.clients {
		if strings.EqualFold(cl.name, targetName) {
			go sendInventoryTo(cl)
			break
		}
	}
	globalHub.mu.RUnlock()

	sendDirectMsg(c, fmt.Sprintf("Removed '%s' from %s.", itemID, targetName))
	log.Printf("[ADMIN] %s removed '%s' from %s", c.name, itemID, targetName)
}

// ──────────────────────────────────────────────────────────────
// Item use
// ──────────────────────────────────────────────────────────────

func handleUseItem(c *Client, raw []byte) {
	var msg struct {
		ItemID string `json:"item_id"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.ItemID == "" {
		return
	}
	def, ok := allItemDefs[msg.ItemID]
	if !ok {
		return
	}
	// Verify the player actually owns the item.
	rows := db.GetInventory(c.userID)
	hasItem := false
	for _, r := range rows {
		if r.ItemID == msg.ItemID && r.Quantity > 0 {
			hasItem = true
			break
		}
	}
	if !hasItem {
		return
	}
	// Update server anim state.
	globalHub.mu.Lock()
	c.state.AnimState = def.AnimState
	globalHub.mu.Unlock()

	// Broadcast immediately so other clients react without waiting for the
	// next 60 Hz state tick.
	globalHub.broadcast(map[string]interface{}{
		"type":      "use_item_ok",
		"player_id": c.playerID,
		"item_id":   msg.ItemID,
		"anim":      def.AnimState,
	})
	log.Printf("[ITEM] %s uses %s", c.name, def.Name)
}

// ──────────────────────────────────────────────────────────────
// Shop / world items
// ──────────────────────────────────────────────────────────────

func handleBuyWorldItem(c *Client, raw []byte) {
	var msg struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.ID == "" {
		return
	}

	// Copy the item under the lock; admins may remove it concurrently.
	// Scoped to the buyer's map so items cannot be bought from another instance.
	myMap := mapOrDefault(c.currentMap)
	var item *WorldSpawnItem
	globalHub.mu.RLock()
	for _, wi := range globalHub.worldItems {
		if wi.ID == msg.ID && mapOrDefault(wi.MapID) == myMap {
			cp := *wi
			item = &cp
			break
		}
	}
	globalHub.mu.RUnlock()

	if item == nil || item.Price <= 0 {
		return
	}

	newTotal, err := db.DeductGralats(c.userID, item.Price)
	if err != nil {
		data, _ := json.Marshal(map[string]interface{}{
			"type":    "buy_result",
			"success": false,
			"msg":     "Pas assez de gralats !",
		})
		select {
		case c.send <- data:
		default:
		}
		return
	}

	globalHub.mu.Lock()
	c.state.Gralats = newTotal
	globalHub.mu.Unlock()

	if item.ItemID != "" {
		db.GiveItem(c.userID, item.ItemID, 1)
		go sendInventoryTo(c)
	}

	data, _ := json.Marshal(map[string]interface{}{
		"type":     "buy_result",
		"success":  true,
		"msg":      fmt.Sprintf("Acheté '%s' pour %d gralats !", item.Name, item.Price),
		"gralat_n": newTotal,
		"item_id":  item.ItemID,
	})
	select {
	case c.send <- data:
	default:
	}
	log.Printf("[SHOP] %s bought '%s' for %d G (balance: %d)", c.name, item.Name, item.Price, newTotal)
}

func handleAdminSpawnWorldItem(c *Client, raw []byte) {
	if !c.isAdmin {
		return
	}
	var msg struct {
		Name       string  `json:"name"`
		SpritePath string  `json:"sprite"`
		X          float64 `json:"x"`
		Y          float64 `json:"y"`
		Price      int     `json:"price"`
		ItemID     string  `json:"item_id"`
	}
	if json.Unmarshal(raw, &msg) != nil || strings.TrimSpace(msg.Name) == "" {
		return
	}

	id := fmt.Sprintf("wi_%d", time.Now().UnixMilli())
	wi := &WorldSpawnItem{
		ID:         id,
		Name:       strings.TrimSpace(msg.Name),
		SpritePath: msg.SpritePath,
		X:          msg.X,
		Y:          msg.Y,
		Price:      msg.Price,
		ItemID:     msg.ItemID,
		// Spawn on the admin's own map, not always the default one.
		MapID: mapOrDefault(c.currentMap),
	}
	globalHub.addWorldItem(wi)
	db.SaveWorldItem(db.WorldItemRow{
		ID: id, Name: wi.Name, SpritePath: wi.SpritePath,
		X: wi.X, Y: wi.Y, Price: wi.Price, ItemID: wi.ItemID,
		MapName: wi.MapID,
	})
	log.Printf("[ADMIN] %s spawned world item '%s' at (%.0f,%.0f) on %s", c.name, wi.Name, wi.X, wi.Y, wi.MapID)
}

func handleAdminRemoveWorldItem(c *Client, raw []byte) {
	if !c.isAdmin {
		return
	}
	var msg struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.ID == "" {
		return
	}
	globalHub.removeWorldItem(msg.ID)
	db.RemoveWorldItem(msg.ID)
	log.Printf("[ADMIN] %s removed world item %s", c.name, msg.ID)
}
