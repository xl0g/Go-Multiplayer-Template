package main

import (
	"darkzone/MultiTestServer/internal/db"
	"encoding/json"
	"fmt"
	"log"
	mrand "math/rand"

	lua "github.com/yuin/gopher-lua"
)

// registerBindings installs the full Lua API into the Lua state for a resource.
// Every function is a closure that captures r (the resource) and lm (the manager).
func (lm *LuaManager) registerBindings(r *Resource) {
	L := r.L
	rName := r.name

	// ── Logging ──────────────────────────────────────────────────────────────
	// Override the built-in print to prefix output with the resource name.
	L.SetGlobal("print", L.NewFunction(func(L *lua.LState) int {
		n := L.GetTop()
		parts := make([]string, n)
		for i := 1; i <= n; i++ {
			parts[i-1] = L.Get(i).String()
		}
		msg := ""
		for i, p := range parts {
			if i > 0 {
				msg += "\t"
			}
			msg += p
		}
		log.Printf("[LUA:%s] %s", rName, msg)
		return 0
	}))

	// ── Timer API ─────────────────────────────────────────────────────────────
	// SetTimeout(ms, fn) → timerId   — fires fn once after ms milliseconds
	L.SetGlobal("SetTimeout", L.NewFunction(func(L *lua.LState) int {
		ms := float64(L.CheckNumber(1))
		fn := L.CheckFunction(2)
		r.timerSeq++
		id := r.timerSeq
		r.timers = append(r.timers, &luaTimer{
			id:       id,
			interval: ms / 1000.0,
			fn:       fn,
			repeat:   false,
		})
		L.Push(lua.LNumber(id))
		return 1
	}))

	// SetInterval(ms, fn) → timerId  — fires fn every ms milliseconds
	L.SetGlobal("SetInterval", L.NewFunction(func(L *lua.LState) int {
		ms := float64(L.CheckNumber(1))
		fn := L.CheckFunction(2)
		r.timerSeq++
		id := r.timerSeq
		r.timers = append(r.timers, &luaTimer{
			id:       id,
			interval: ms / 1000.0,
			fn:       fn,
			repeat:   true,
		})
		L.Push(lua.LNumber(id))
		return 1
	}))

	// ClearTimer(id)  — cancels a SetTimeout or SetInterval
	L.SetGlobal("ClearTimer", L.NewFunction(func(L *lua.LState) int {
		id := int(L.CheckNumber(1))
		for _, t := range r.timers {
			if t.id == id {
				t.dead = true
				break
			}
		}
		return 0
	}))

	// ── Event API ─────────────────────────────────────────────────────────────
	// AddEventHandler(eventName, fn)
	// Built-in events: onServerStart, onResourceStop,
	//                  onPlayerConnect, onPlayerDisconnect, onPlayerChat
	L.SetGlobal("AddEventHandler", L.NewFunction(func(L *lua.LState) int {
		event := L.CheckString(1)
		fn := L.CheckFunction(2)
		r.handlers[event] = append(r.handlers[event], fn)
		return 0
	}))

	// TriggerEvent(eventName, ...)  — broadcast to all resources (queued)
	L.SetGlobal("TriggerEvent", L.NewFunction(func(L *lua.LState) int {
		event := L.CheckString(1)
		n := L.GetTop()
		args := make([]interface{}, n-1)
		for i := 2; i <= n; i++ {
			args[i-2] = luaValToGo(L.Get(i))
		}
		if globalLuaManager != nil {
			globalLuaManager.TriggerEvent(event, args...)
		}
		return 0
	}))

	// ── Resource info ─────────────────────────────────────────────────────────
	// GetResourceName() → string
	L.SetGlobal("GetResourceName", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(rName))
		return 1
	}))

	// ── NPC API ───────────────────────────────────────────────────────────────
	// CreateNPC(name, x, y [, npcType=0 [, dialog="" [, gMin=1 [, gMax=3]]]]) → npcId
	//   npcType: 0=villager 1=merchant 2=guard 3=traveler 4=farmer 5=horse
	L.SetGlobal("CreateNPC", L.NewFunction(func(L *lua.LState) int {
		npcName := L.CheckString(1)
		x := float64(L.CheckNumber(2))
		y := float64(L.CheckNumber(3))
		npcType := int(L.OptNumber(4, 0))
		dialog := L.OptString(5, "")
		gMin := int(L.OptNumber(6, 1))
		gMax := int(L.OptNumber(7, 3))

		id := fmt.Sprintf("lua_%s_%d", rName, len(r.spawnedNPCs)+1)
		npc := newNPC(id, npcName, x, y, npcType)
		if dialog != "" {
			npc.customDialog = dialog
			npc.customGMin = gMin
			npc.customGMax = gMax
		}
		lm.hub.addLuaNPC(npc)
		r.spawnedNPCs = append(r.spawnedNPCs, id)

		log.Printf("[LUA:%s] CreateNPC '%s' (type=%d) at (%.0f,%.0f) → %s", rName, npcName, npcType, x, y, id)
		L.Push(lua.LString(id))
		return 1
	}))

	// DeleteNPC(npcId)
	L.SetGlobal("DeleteNPC", L.NewFunction(func(L *lua.LState) int {
		id := L.CheckString(1)
		lm.hub.removeLuaNPC(id)
		for i, sid := range r.spawnedNPCs {
			if sid == id {
				r.spawnedNPCs = append(r.spawnedNPCs[:i], r.spawnedNPCs[i+1:]...)
				break
			}
		}
		return 0
	}))

	// SetNPCPosition(npcId, x, y)
	L.SetGlobal("SetNPCPosition", L.NewFunction(func(L *lua.LState) int {
		id := L.CheckString(1)
		x := float64(L.CheckNumber(2))
		y := float64(L.CheckNumber(3))
		lm.hub.setLuaNPCPos(id, x, y)
		return 0
	}))

	// SetNPCDialog(npcId, message [, gMin=1 [, gMax=3]])
	L.SetGlobal("SetNPCDialog", L.NewFunction(func(L *lua.LState) int {
		id := L.CheckString(1)
		msg := L.CheckString(2)
		gMin := int(L.OptNumber(3, 1))
		gMax := int(L.OptNumber(4, 3))
		lm.hub.setLuaNPCDialog(id, msg, gMin, gMax)
		return 0
	}))

	// SetNPCBehaviour(npcId, name)
	// name ∈ "wander" | "roam" | "patrol" | "passive" | "aggressive" | "static".
	// Unknown names fall back to "wander".
	L.SetGlobal("SetNPCBehaviour", L.NewFunction(func(L *lua.LState) int {
		id := L.CheckString(1)
		name := L.CheckString(2)
		L.Push(lua.LBool(lm.hub.setLuaNPCBehaviour(id, ParseBehaviour(name))))
		return 1
	}))

	// SetNPCWaypoints(npcId, { {x=100,y=200}, {x=300,y=200}, ... })
	// Installs a patrol route and switches the NPC to patrolling.
	// An empty table reverts it to wandering.
	L.SetGlobal("SetNPCWaypoints", L.NewFunction(func(L *lua.LState) int {
		id := L.CheckString(1)
		tbl := L.CheckTable(2)
		var pts []vec2
		tbl.ForEach(func(_, v lua.LValue) {
			pt, ok := v.(*lua.LTable)
			if !ok {
				return
			}
			x, xok := pt.RawGetString("x").(lua.LNumber)
			y, yok := pt.RawGetString("y").(lua.LNumber)
			if xok && yok {
				pts = append(pts, vec2{X: float64(x), Y: float64(y)})
			}
		})
		L.Push(lua.LBool(lm.hub.setLuaNPCWaypoints(id, pts)))
		return 1
	}))

	// GetNPCPosition(npcId) → x, y  (returns nil, nil if not found)
	L.SetGlobal("GetNPCPosition", L.NewFunction(func(L *lua.LState) int {
		id := L.CheckString(1)
		x, y, ok := lm.hub.getLuaNPCPos(id)
		if !ok {
			L.Push(lua.LNil)
			L.Push(lua.LNil)
		} else {
			L.Push(lua.LNumber(x))
			L.Push(lua.LNumber(y))
		}
		return 2
	}))

	// ── Player API ────────────────────────────────────────────────────────────
	// GetPlayers() → table of {id, name, x, y, gralats}
	L.SetGlobal("GetPlayers", L.NewFunction(func(L *lua.LState) int {
		tbl := L.NewTable()
		lm.hub.mu.RLock()
		i := 1
		for c := range lm.hub.clients {
			row := L.NewTable()
			L.SetField(row, "id", lua.LString(c.playerID))
			L.SetField(row, "name", lua.LString(c.name))
			L.SetField(row, "x", lua.LNumber(c.state.X))
			L.SetField(row, "y", lua.LNumber(c.state.Y))
			L.SetField(row, "gralats", lua.LNumber(c.state.Gralats))
			tbl.RawSetInt(i, row)
			i++
		}
		lm.hub.mu.RUnlock()
		L.Push(tbl)
		return 1
	}))

	// GetPlayerName(playerId) → string | nil
	L.SetGlobal("GetPlayerName", L.NewFunction(func(L *lua.LState) int {
		id := L.CheckString(1)
		lm.hub.mu.RLock()
		defer lm.hub.mu.RUnlock()
		for c := range lm.hub.clients {
			if c.playerID == id {
				L.Push(lua.LString(c.name))
				return 1
			}
		}
		L.Push(lua.LNil)
		return 1
	}))

	// GetPlayerPos(playerId) → x, y  (returns nil, nil if not found)
	L.SetGlobal("GetPlayerPos", L.NewFunction(func(L *lua.LState) int {
		id := L.CheckString(1)
		lm.hub.mu.RLock()
		defer lm.hub.mu.RUnlock()
		for c := range lm.hub.clients {
			if c.playerID == id {
				L.Push(lua.LNumber(c.state.X))
				L.Push(lua.LNumber(c.state.Y))
				return 2
			}
		}
		L.Push(lua.LNil)
		L.Push(lua.LNil)
		return 2
	}))

	// GiveGralats(playerId, amount)
	L.SetGlobal("GiveGralats", L.NewFunction(func(L *lua.LState) int {
		id := L.CheckString(1)
		amount := int(L.CheckNumber(2))
		if amount <= 0 {
			return 0
		}
		lm.hub.mu.RLock()
		var target *Client
		for c := range lm.hub.clients {
			if c.playerID == id {
				target = c
				break
			}
		}
		lm.hub.mu.RUnlock()
		if target == nil {
			return 0
		}
		newTotal, _ := db.AddGralats(target.userID, amount)
		lm.hub.mu.Lock()
		target.state.Gralats = newTotal
		lm.hub.mu.Unlock()
		if data, err := json.Marshal(map[string]interface{}{
			"type": "gralat_update", "gralat_n": newTotal,
		}); err == nil {
			select {
			case target.send <- data:
			default:
			}
		}
		return 0
	}))

	// TakeGralats(playerId, amount) → bool (false if not enough gralats)
	L.SetGlobal("TakeGralats", L.NewFunction(func(L *lua.LState) int {
		id := L.CheckString(1)
		amount := int(L.CheckNumber(2))
		if amount <= 0 {
			L.Push(lua.LTrue)
			return 1
		}
		lm.hub.mu.RLock()
		var target *Client
		for c := range lm.hub.clients {
			if c.playerID == id {
				target = c
				break
			}
		}
		lm.hub.mu.RUnlock()
		if target == nil {
			L.Push(lua.LFalse)
			return 1
		}
		newTotal, err := db.DeductGralats(target.userID, amount)
		if err != nil {
			L.Push(lua.LFalse)
			return 1
		}
		lm.hub.mu.Lock()
		target.state.Gralats = newTotal
		lm.hub.mu.Unlock()
		if data, merr := json.Marshal(map[string]interface{}{
			"type": "gralat_update", "gralat_n": newTotal,
		}); merr == nil {
			select {
			case target.send <- data:
			default:
			}
		}
		L.Push(lua.LTrue)
		return 1
	}))

	// ── Messaging API ─────────────────────────────────────────────────────────
	// SendMessage(playerId, msg)  — system message to one player; "-1" = all
	L.SetGlobal("SendMessage", L.NewFunction(func(L *lua.LState) int {
		id := L.CheckString(1)
		msg := L.CheckString(2)
		if id == "-1" {
			lm.hub.broadcastSystem(msg)
			return 0
		}
		lm.hub.mu.RLock()
		defer lm.hub.mu.RUnlock()
		for c := range lm.hub.clients {
			if c.playerID == id {
				sendDirectMsg(c, msg)
				break
			}
		}
		return 0
	}))

	// BroadcastMessage(msg)  — system message to all connected players
	L.SetGlobal("BroadcastMessage", L.NewFunction(func(L *lua.LState) int {
		msg := L.CheckString(1)
		lm.hub.broadcastSystem(msg)
		return 0
	}))

	// BroadcastChat(from, msg)  — broadcast a chat message attributed to from
	L.SetGlobal("BroadcastChat", L.NewFunction(func(L *lua.LState) int {
		from := L.CheckString(1)
		msg := L.CheckString(2)
		lm.hub.broadcast(map[string]interface{}{
			"type": "chat", "from": from, "msg": msg,
		})
		return 0
	}))

	// ── Utility ───────────────────────────────────────────────────────────────
	// RandomInt(min, max) → int  (inclusive on both ends)
	L.SetGlobal("RandomInt", L.NewFunction(func(L *lua.LState) int {
		min := int(L.CheckNumber(1))
		max := int(L.CheckNumber(2))
		if max <= min {
			L.Push(lua.LNumber(min))
		} else {
			L.Push(lua.LNumber(min + mrand.Intn(max-min+1)))
		}
		return 1
	}))
}

// luaValToGo converts a Lua scalar to a Go interface for event passing.
func luaValToGo(v lua.LValue) interface{} {
	switch v := v.(type) {
	case lua.LBool:
		return bool(v)
	case lua.LNumber:
		return float64(v)
	case lua.LString:
		return string(v)
	default:
		return nil
	}
}
