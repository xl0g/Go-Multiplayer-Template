package main

import (
	"math"
	mrand "math/rand"
)

const (
	mapWidth  = 1120.0
	mapHeight = 1120.0

	// NPC type constants (must match client-side values)
	NPCTypeVillager     = 0
	NPCTypeMerchant     = 1
	NPCTypeGuard        = 2
	NPCTypeTraveler     = 3
	NPCTypeFarmer       = 4
	NPCTypeHorse        = 5
	NPCTypeAggressive   = 6
	NPCTypePassive      = 7
	NPCTypeSpawnedEnemy = 8

	// Aggressive NPC
	aggroRange      = 160.0
	aggroAttackDist = 32.0
	aggroAttackCD   = 1.2
	aggroDamage     = 1
	aggroSpeed      = 90.0

	// Aggro leash: how far an NPC may be dragged from its home before it gives
	// up and walks back. Without a leash a player can pull a monster across the
	// whole world and strand it there.
	aggroLeashRange   = 420.0
	returnHomeSpeedMul = 1.35 // NPCs head home a bit faster than they chase
	returnHomeDoneDist = 24.0 // close enough to home to resume normal behaviour

	// Alerting: when an NPC starts chasing, nearby allies join in.
	alertRadius   = 190.0
	alertCooldown = 6.0 // seconds before the same NPC can raise another alert

	// Patrol
	patrolWaypointDist  = 12.0 // distance at which a waypoint counts as reached
	patrolPauseMin      = 0.6
	patrolPauseMax      = 2.0
	patrolDefaultRadius = 160.0 // radius of the generated 4-point patrol square

	// Roaming: like wandering but over a much wider area, and rarely idle.
	roamRadius     = 900.0
	roamPauseMax   = 1.2
	roamSpeedMul   = 1.1

	// Admin-spawned enemy
	spawnedEnemySpeed      = 240.0
	spawnedEnemyAttackDist = 52.0
	spawnedEnemyAggroRange = 800.0
	spawnedEnemyHP         = 6

	// Passive NPC
	passiveFleeRange = 100.0
	passiveFleeSpeed = 120.0

	// NPC bounding box used for every collision test. Spawn placement must use
	// the same box as movement, or an NPC can be placed inside a wall and be
	// unable to move at all.
	npcW = 28.0
	npcH = 28.0

	// How long an NPC may be completely blocked before its behaviour gives up on
	// the current destination and picks another.
	// Minimum reduction in distance-to-target that counts as making progress.
	// Small enough not to reject genuinely slow movement, large enough that
	// floating-point noise never reads as progress.
	progressEpsilon = 0.01

	blockedRetargetWander = 0.8
	blockedRetargetRoam   = 1.5
	blockedRetargetPatrol = 2.5
	blockedGiveUpReturn   = 4.0
)

// Behaviour describes how an NPC acts when it is not reacting to combat.
// It is deliberately separate from NPCType: the type decides how an NPC looks
// and what it says, the behaviour decides how it moves. A merchant can patrol
// and a guard can stand still without inventing new types for each combination.
type Behaviour int

const (
	BehaviourWander     Behaviour = iota // small random walk around home (default)
	BehaviourRoam                        // wide random walk, rarely idle
	BehaviourPatrol                      // walks a fixed waypoint loop
	BehaviourPassive                     // flees nearby players
	BehaviourAggressive                  // chases and attacks players
	BehaviourStatic                      // never moves
)

func (b Behaviour) String() string {
	switch b {
	case BehaviourRoam:
		return "roam"
	case BehaviourPatrol:
		return "patrol"
	case BehaviourPassive:
		return "passive"
	case BehaviourAggressive:
		return "aggressive"
	case BehaviourStatic:
		return "static"
	default:
		return "wander"
	}
}

// ParseBehaviour maps a name (as used by Lua scripts) to a Behaviour.
// Unknown names fall back to wandering.
func ParseBehaviour(s string) Behaviour {
	switch s {
	case "roam":
		return BehaviourRoam
	case "patrol":
		return BehaviourPatrol
	case "passive":
		return BehaviourPassive
	case "aggressive":
		return BehaviourAggressive
	case "static":
		return BehaviourStatic
	default:
		return BehaviourWander
	}
}

// defaultBehaviourFor keeps the historical mapping from NPC type to behaviour,
// so existing NPC definitions keep acting the way they used to.
func defaultBehaviourFor(npcType int) Behaviour {
	switch npcType {
	case NPCTypeAggressive, NPCTypeSpawnedEnemy:
		return BehaviourAggressive
	case NPCTypePassive:
		return BehaviourPassive
	case NPCTypeGuard:
		return BehaviourPatrol
	default:
		return BehaviourWander
	}
}

// aiState tracks what the NPC is doing right now, which may override its
// baseline behaviour (a patrolling guard that spots a player switches to
// chasing, then to returning, then back to patrolling).
type aiState int

const (
	aiNormal aiState = iota
	aiChasing
	aiReturning
)

// vec2 is a world-space point.
type vec2 struct{ X, Y float64 }

// NPC is a server-side entity with shared combat logic (via CombatEntity) and AI.
type NPC struct {
	combat       CombatEntity // HP, damage, invulnerability, respawn — identical to player
	state        NPCState     // wire-format snapshot sent to clients; synced before broadcast
	homeX, homeY float64      // respawn / wander anchor position
	worldW, worldH float64    // world bounds for movement clamping
	speed        float64
	targetX      float64
	targetY      float64
	timer        float64 // wander: wait time before choosing next target

	mapID     string // map instance this NPC belongs to (empty = defaultMap)
	mountedBy string // player ID currently riding this NPC (horses only)

	stuckTimer    float64 // time spent continuously blocked against a wall
	stuckAngle    float64 // committed detour direction while unsticking
	stuckAngleSet bool    // whether stuckAngle currently holds a chosen detour
	// blockedTime accumulates for as long as the NPC fails to get any closer to
	// its destination than it has already been. Unlike stuckTimer it is not reset
	// by the detour logic, so behaviours can tell "briefly scraping a wall" from
	// "this destination is unreachable".
	blockedTime float64
	// bestDist is the closest the NPC has come to progTarget so far. Progress is
	// measured against this best-so-far rather than the previous tick, otherwise
	// an NPC oscillating along a wall resets the timer every time it swings back.
	bestDist               float64
	progTargetX, progTargetY float64

	// Optional Lua-defined dialog
	customDialog         string
	customGMin, customGMax int

	aggroTarget string // player ID being chased (aggressive NPCs)

	// Behaviour and AI state machine
	behaviour  Behaviour
	state2     aiState
	leashRange float64 // max distance from home while chasing (0 = default)

	// Patrol route, in world coordinates. Empty for non-patrolling NPCs.
	waypoints []vec2
	wpIndex   int
	wpPause   float64 // remaining pause at the current waypoint

	// Alerting
	alertCD      float64 // seconds until this NPC may raise another alert
	raisedAlert  bool    // set for one tick when this NPC starts a chase
	alertedByAlly bool   // set by the hub when an ally raised an alert nearby
}

func newNPC(id, name string, x, y float64, npcType int) *NPC {
	maxHP := 5
	speed := 70.0 + mrand.Float64()*50.0

	switch npcType {
	case NPCTypeHorse:
		maxHP = 0 // immortal
		speed = 55.0 + mrand.Float64()*30.0
	case NPCTypeAggressive:
		maxHP = 8
		speed = aggroSpeed + mrand.Float64()*20.0
	case NPCTypePassive:
		maxHP = 3
		speed = passiveFleeSpeed + mrand.Float64()*20.0
	case NPCTypeSpawnedEnemy:
		maxHP = spawnedEnemyHP
		speed = spawnedEnemySpeed + mrand.Float64()*15.0
	}

	n := &NPC{
		combat: newCombat(maxHP),
		state: NPCState{
			ID:      id,
			Name:    name,
			X:       x,
			Y:       y,
			Dir:     mrand.Intn(4),
			NPCType: npcType,
		},
		homeX:     x,
		homeY:     y,
		worldW:    mapWidth,
		worldH:    mapHeight,
		speed:     speed,
		targetX:   x,
		targetY:   y,
		timer:     mrand.Float64() * 3.0,
		behaviour: defaultBehaviourFor(npcType),
	}
	if n.behaviour == BehaviourPatrol {
		n.waypoints = squarePatrol(x, y, patrolDefaultRadius)
	}
	n.syncState()
	return n
}

// squarePatrol generates a simple four-corner route around (x, y).
// Used as the default route for guards so patrolling works without every
// caller having to author waypoints.
func squarePatrol(x, y, r float64) []vec2 {
	return []vec2{
		{x - r, y - r},
		{x + r, y - r},
		{x + r, y + r},
		{x - r, y + r},
	}
}

// SetBehaviour changes how the NPC moves. Switching to patrol without a route
// generates the default square, so a caller can just ask for "patrol".
func (n *NPC) SetBehaviour(b Behaviour) {
	n.behaviour = b
	n.state2 = aiNormal
	n.aggroTarget = ""
	if b == BehaviourPatrol && len(n.waypoints) == 0 {
		n.waypoints = squarePatrol(n.homeX, n.homeY, patrolDefaultRadius)
	}
}

// SetWaypoints installs a patrol route (world coordinates) and switches the NPC
// to patrolling. An empty route reverts it to wandering.
func (n *NPC) SetWaypoints(pts []vec2) {
	n.waypoints = pts
	n.wpIndex = 0
	n.wpPause = 0
	if len(pts) == 0 {
		n.behaviour = BehaviourWander
		return
	}
	n.behaviour = BehaviourPatrol
	n.state2 = aiNormal
}

// raiseAlert marks this NPC as calling nearby allies for help, respecting the
// per-NPC cooldown so a long fight does not re-alert every tick.
// The hub's propagation pass consumes the flag.
func (n *NPC) raiseAlert() {
	if n.alertCD <= 0 {
		n.raisedAlert = true
		n.alertCD = alertCooldown
	}
}

// provoke makes the NPC react to being attacked: anything that is not passive
// turns on its attacker, and either way nearby allies are called in.
func (n *NPC) provoke() {
	if n.behaviour != BehaviourPassive && n.state2 == aiNormal {
		n.state2 = aiChasing
	}
	n.raiseAlert()
}

// leash returns the effective leash distance for this NPC.
func (n *NPC) leash() float64 {
	if n.leashRange > 0 {
		return n.leashRange
	}
	if n.state.NPCType == NPCTypeSpawnedEnemy {
		// Admin-spawned hunters are meant to be relentless.
		return spawnedEnemyAggroRange
	}
	return aggroLeashRange
}

// syncState copies combat HP/alive status into the wire-format NPCState.
// Call after every combat update and before broadcasting.
func (n *NPC) syncState() {
	n.state.HP = n.combat.HP
	n.state.MaxHP = n.combat.MaxHP
	if !n.combat.alive {
		n.state.AnimState = "dead"
	} else if n.state.AnimState == "dead" {
		n.state.AnimState = ""
	}
}

// playerPos is a lightweight snapshot of a connected player used by AI.
type playerPos struct {
	id    string
	x, y  float64
	alive bool
}

// update advances the NPC's AI by dt seconds.
// Returns a non-empty playerID if this NPC just attacked that player.
func (n *NPC) update(dt float64, collMap WorldCollider, players []playerPos) (attackedID string) {
	// Tick shared combat (cooldowns + respawn).
	if respawned := n.combat.Tick(dt); respawned {
		n.state.X = n.homeX
		n.state.Y = n.homeY
		n.targetX = n.homeX
		n.targetY = n.homeY
		n.aggroTarget = ""
		n.timer = 2.0
		n.syncState()
		return ""
	}

	if !n.combat.alive {
		n.syncState()
		return ""
	}

	// Horses are driven by the rider's move messages.
	if n.mountedBy != "" {
		n.state.Moving = false
		return ""
	}

	if n.alertCD > 0 {
		n.alertCD -= dt
	}
	// raisedAlert is deliberately NOT cleared here: damageNPC can set it between
	// ticks, and the hub's propagation pass (which runs after this) is what
	// consumes and clears it.

	// An ally's alert promotes a non-aggressive NPC to chasing for this fight.
	if n.alertedByAlly {
		n.alertedByAlly = false
		if n.behaviour != BehaviourPassive && n.state2 == aiNormal {
			n.state2 = aiChasing
		}
	}

	// Returning home overrides the baseline behaviour until it completes.
	if n.state2 == aiReturning {
		n.updateReturnHome(dt, collMap)
		n.syncState()
		return ""
	}

	switch n.behaviour {
	case BehaviourAggressive:
		attackedID = n.updateAggressive(dt, collMap, players)
	case BehaviourPassive:
		n.updatePassive(dt, collMap, players)
	case BehaviourPatrol:
		// A patrolling NPC that was alerted chases; otherwise it walks its route.
		if n.state2 == aiChasing {
			attackedID = n.updateAggressive(dt, collMap, players)
		} else {
			n.updatePatrol(dt, collMap)
		}
	case BehaviourRoam:
		if n.state2 == aiChasing {
			attackedID = n.updateAggressive(dt, collMap, players)
		} else {
			n.updateRoam(dt, collMap)
		}
	case BehaviourStatic:
		n.state.Moving = false
	default:
		if n.state2 == aiChasing {
			attackedID = n.updateAggressive(dt, collMap, players)
		} else {
			n.updateWander(dt, collMap)
		}
	}
	n.syncState()
	return attackedID
}

func (n *NPC) updateAggressive(dt float64, collMap WorldCollider, players []playerPos) string {
	isSpawned := n.state.NPCType == NPCTypeSpawnedEnemy
	effectiveRange := aggroRange
	attackDist := aggroAttackDist
	if isSpawned {
		effectiveRange = spawnedEnemyAggroRange
		attackDist = spawnedEnemyAttackDist
	}

	// Find nearest alive player within aggro range.
	nearestDist := math.MaxFloat64
	var nearestID string
	var nearestX, nearestY float64
	for _, p := range players {
		if !p.alive {
			continue
		}
		dx := p.x - n.state.X
		dy := p.y - n.state.Y
		d := math.Sqrt(dx*dx + dy*dy)
		if d < nearestDist {
			nearestDist = d
			nearestID = p.id
			nearestX = p.x
			nearestY = p.y
		}
	}

	// Already-chasing NPCs keep their target a bit beyond the initial aggro
	// range, so a player cannot break the chase by stepping one pixel back.
	if n.state2 == aiChasing {
		effectiveRange *= 1.5
	}

	if nearestID == "" || nearestDist > effectiveRange {
		wasChasing := n.state2 == aiChasing
		n.aggroTarget = ""
		n.stuckTimer = 0
		if wasChasing {
			// Lost the target — walk back home rather than wandering off from
			// wherever the chase ended.
			n.state2 = aiReturning
			n.updateReturnHome(dt, collMap)
			return ""
		}
		n.baselineMove(dt, collMap)
		return ""
	}

	// Leash: if the chase has dragged us too far from home, give up and return.
	hdx, hdy := n.state.X-n.homeX, n.state.Y-n.homeY
	if leash := n.leash(); hdx*hdx+hdy*hdy > leash*leash {
		n.aggroTarget = ""
		n.state2 = aiReturning
		n.updateReturnHome(dt, collMap)
		return ""
	}

	// Entering a chase raises an alert so nearby allies join in (hub-driven).
	if n.state2 != aiChasing {
		n.raiseAlert()
	}
	n.state2 = aiChasing
	n.aggroTarget = nearestID
	n.state.Moving = true

	// Attack if close enough and cooldown expired.
	if nearestDist <= attackDist && n.combat.CanAttack() {
		n.combat.atkCD = aggroAttackCD
		return nearestID
	}

	// Move toward the player.
	n.moveToward(dt, collMap, nearestX, nearestY, n.speed, 1.0)
	return ""
}

// moveToward advances the NPC toward (tx, ty), splitting the X and Y steps so it
// slides along walls instead of stopping dead against them.
// Returns true once the NPC is within stopDist of the target.
func (n *NPC) moveToward(dt float64, collMap WorldCollider, tx, ty, speed, stopDist float64) bool {
	dx, dy := tx-n.state.X, ty-n.state.Y
	dist := math.Sqrt(dx*dx + dy*dy)
	if dist <= stopDist {
		n.state.Moving = false
		n.stuckTimer = 0
		n.stuckAngleSet = false
		n.blockedTime = 0
		n.bestDist = 0
		return true
	}

	// Restart the progress baseline whenever the destination really changes.
	// The threshold keeps a chased player's small movements from continually
	// resetting stall detection, while a genuinely new destination does reset it.
	if math.Hypot(tx-n.progTargetX, ty-n.progTargetY) > 32 {
		n.progTargetX, n.progTargetY = tx, ty
		n.bestDist = math.MaxFloat64
	}
	n.state.Moving = true

	step := n.speed * dt
	if speed > 0 {
		step = speed * dt
	}
	if step > dist {
		step = dist // don't overshoot and oscillate around the target
	}
	newX := clamp(n.state.X+(dx/dist)*step, 1, n.worldW-npcW-1)
	newY := clamp(n.state.Y+(dy/dist)*step, 1, n.worldH-npcH-1)

	if canMove(collMap, newX, n.state.Y, npcW, npcH) {
		n.state.X = newX
	}
	if canMove(collMap, n.state.X, newY, npcW, npcH) {
		n.state.Y = newY
	}

	// Progress means getting closer to the target — not that a collision test
	// passed, and not that the position changed at all. Two ways this used to go
	// wrong and leave an NPC frozen while reporting Moving = true:
	//   - On an axis-aligned leg (dy == 0, which every squarePatrol leg is) the
	//     candidate Y equals the current Y, so the Y collision test trivially
	//     passes on the position the NPC already occupies.
	//   - While unsticking, the sideways detour changes the position without
	//     bringing the NPC any nearer, which also read as movement.
	// Both reset the blocked timers every tick, so no behaviour ever gave up on
	// an unreachable destination.
	if d := math.Hypot(tx-n.state.X, ty-n.state.Y); d < n.bestDist-progressEpsilon {
		n.bestDist = d
		n.stuckTimer = 0
		n.stuckAngleSet = false
		n.blockedTime = 0
	} else {
		n.blockedTime += dt
		n.unstick(dt, collMap, dx, dy, step)
	}
	n.setDirFromDelta(dx, dy)
	return false
}

// unstick handles an NPC pressed flat against a wall. After a short delay it
// commits to one perpendicular detour direction and keeps it, rather than
// re-rolling a random angle every tick — which made NPCs vibrate in place
// against corners instead of walking around them.
func (n *NPC) unstick(dt float64, collMap WorldCollider, dx, dy, step float64) {
	n.stuckTimer += dt
	if n.stuckTimer < 0.25 {
		return
	}
	if !n.stuckAngleSet {
		base := math.Atan2(dy, dx)
		if mrand.Intn(2) == 0 {
			n.stuckAngle = base + math.Pi/2
		} else {
			n.stuckAngle = base - math.Pi/2
		}
		n.stuckAngleSet = true
	}

	prevX, prevY := n.state.X, n.state.Y
	sx := clamp(n.state.X+math.Cos(n.stuckAngle)*step, 1, n.worldW-npcW-1)
	sy := clamp(n.state.Y+math.Sin(n.stuckAngle)*step, 1, n.worldH-npcH-1)
	if canMove(collMap, sx, n.state.Y, npcW, npcH) {
		n.state.X = sx
	}
	if canMove(collMap, n.state.X, sy, npcW, npcH) {
		n.state.Y = sy
	}
	// Same rule as moveToward: only an actual change counts as escaping.
	moved := n.state.X != prevX || n.state.Y != prevY
	// Detour exhausted or not working — drop it so the other side is tried next.
	if !moved || n.stuckTimer > 1.5 {
		n.stuckAngleSet = false
		n.stuckTimer = 0
	}
}

// baselineMove runs the NPC's non-combat behaviour for one tick.
func (n *NPC) baselineMove(dt float64, collMap WorldCollider) {
	switch n.behaviour {
	case BehaviourPatrol:
		n.updatePatrol(dt, collMap)
	case BehaviourRoam:
		n.updateRoam(dt, collMap)
	case BehaviourStatic:
		n.state.Moving = false
	default:
		n.updateWander(dt, collMap)
	}
}

// updateReturnHome walks the NPC back to its anchor after a chase ends, so a
// monster dragged across the map does not simply take up residence where the
// player left it.
func (n *NPC) updateReturnHome(dt float64, collMap WorldCollider) {
	if n.moveToward(dt, collMap, n.homeX, n.homeY, n.speed*returnHomeSpeedMul, returnHomeDoneDist) {
		n.finishReturn()
		return
	}
	// Home is unreachable (geometry changed, or the NPC was pushed somewhere
	// walled off). Resume normal behaviour where it stands rather than grinding
	// against a wall forever.
	if n.blockedTime > blockedGiveUpReturn {
		n.homeX, n.homeY = n.state.X, n.state.Y
		n.finishReturn()
	}
}

// finishReturn hands control back to the NPC's baseline behaviour.
func (n *NPC) finishReturn() {
	n.state2 = aiNormal
	n.targetX, n.targetY = n.state.X, n.state.Y
	n.timer = 0.5
	n.wpPause = 0
	n.blockedTime = 0
}

// updatePatrol walks the waypoint loop, pausing briefly at each stop.
func (n *NPC) updatePatrol(dt float64, collMap WorldCollider) {
	if len(n.waypoints) == 0 {
		n.updateWander(dt, collMap)
		return
	}
	if n.wpPause > 0 {
		n.wpPause -= dt
		n.state.Moving = false
		return
	}
	wp := n.waypoints[n.wpIndex%len(n.waypoints)]
	tx := clamp(wp.X, 1, n.worldW-32)
	ty := clamp(wp.Y, 1, n.worldH-32)
	reached := n.moveToward(dt, collMap, tx, ty, n.speed, patrolWaypointDist)
	// Skip a waypoint that turns out to be unreachable, so one bad corner does
	// not park the guard against a wall for the rest of the session.
	if reached || n.blockedTime > blockedRetargetPatrol {
		n.wpIndex = (n.wpIndex + 1) % len(n.waypoints)
		n.wpPause = patrolPauseMin + mrand.Float64()*(patrolPauseMax-patrolPauseMin)
		n.blockedTime = 0
	}
}

// updateRoam is a wide-area random walk: same idea as wandering, but over
// roamRadius and with almost no idling, so the NPC reads as a traveller rather
// than a villager pacing outside their door.
func (n *NPC) updateRoam(dt float64, collMap WorldCollider) {
	if !n.moveToward(dt, collMap, n.targetX, n.targetY, n.speed*roamSpeedMul, 8.0) {
		// The destination is walled off: pick another instead of pressing against
		// the obstacle until the end of time.
		if n.blockedTime > blockedRetargetRoam {
			n.pickRoamTarget()
			n.blockedTime = 0
		}
		return
	}
	n.timer -= dt
	if n.timer > 0 {
		return
	}
	n.pickRoamTarget()
}

// pickRoamTarget chooses a new wide-area destination around the NPC's home.
func (n *NPC) pickRoamTarget() {
	angle := mrand.Float64() * math.Pi * 2
	radius := 200.0 + mrand.Float64()*(roamRadius-200.0)
	n.targetX = clamp(n.homeX+math.Cos(angle)*radius, 50, n.worldW-50)
	n.targetY = clamp(n.homeY+math.Sin(angle)*radius, 50, n.worldH-50)
	n.timer = mrand.Float64() * roamPauseMax
}

func (n *NPC) updatePassive(dt float64, collMap WorldCollider, players []playerPos) {
	nearestDist := math.MaxFloat64
	var nearestX, nearestY float64
	for _, p := range players {
		dx := p.x - n.state.X
		dy := p.y - n.state.Y
		d := math.Sqrt(dx*dx + dy*dy)
		if d < nearestDist {
			nearestDist = d
			nearestX = p.x
			nearestY = p.y
		}
	}

	if nearestDist > passiveFleeRange {
		n.updateWander(dt, collMap)
		return
	}

	dx := n.state.X - nearestX
	dy := n.state.Y - nearestY
	dist := math.Sqrt(dx*dx + dy*dy)
	if dist < 1 {
		n.state.Moving = false
		return
	}
	// Flee toward a point directly away from the player, going through
	// moveToward so wall sliding and stuck detection are shared with every other
	// behaviour rather than reimplemented here.
	fleeX := n.state.X + (dx/dist)*passiveFleeRange*2
	fleeY := n.state.Y + (dy/dist)*passiveFleeRange*2
	n.moveToward(dt, collMap, fleeX, fleeY, passiveFleeSpeed, 1.0)
}

func (n *NPC) updateWander(dt float64, collMap WorldCollider) {
	if !n.moveToward(dt, collMap, n.targetX, n.targetY, n.speed, 4.0) {
		// Destination unreachable — pick a different one instead of grinding.
		if n.blockedTime > blockedRetargetWander {
			n.pickWanderTarget()
			n.blockedTime = 0
		}
		return
	}
	n.timer -= dt
	if n.timer <= 0 {
		n.pickWanderTarget()
	}
}

// pickWanderTarget chooses a new short-range destination around home and a new
// idle delay before the one after that.
func (n *NPC) pickWanderTarget() {
	angle := mrand.Float64() * math.Pi * 2
	radius := 80.0 + mrand.Float64()*150.0
	n.targetX = clamp(n.homeX+math.Cos(angle)*radius, 50, n.worldW-50)
	n.targetY = clamp(n.homeY+math.Sin(angle)*radius, 50, n.worldH-50)
	n.timer = 1.5 + mrand.Float64()*4.0
}

func (n *NPC) setDirFromDelta(dx, dy float64) {
	if math.Abs(dx) > math.Abs(dy) {
		if dx > 0 {
			n.state.Dir = 3
		} else {
			n.state.Dir = 1
		}
	} else {
		if dy > 0 {
			n.state.Dir = 2
		} else {
			n.state.Dir = 0
		}
	}
}

// canMove returns true when the bounding box at (x,y) is passable.
// A nil collider means the world has no collision — always passable.
func canMove(collMap WorldCollider, x, y, w, h float64) bool {
	return collMap == nil || !collMap.IsBlocked(x, y, w, h)
}

// clamp constrains v to [lo, hi].
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// npcDialogDefs maps NPC type → conversation text and gralat reward.
var npcDialogDefs = []struct {
	msg        string
	minG, maxG int
}{
	{"Greetings, traveller! The village is peaceful today. Take these gralats for your journey.", 1, 3},
	{"Great deal! Special price just for you, friend.", 2, 5},
	{"Halt! Hmm... you seem harmless enough. Take these gralats and be on your way.", 1, 2},
	{"I have returned from distant lands! I gladly share my findings with fellow travellers.", 1, 5},
	{"What a harvest this season! Here is your share, friend.", 2, 4},
}
