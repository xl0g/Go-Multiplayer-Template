package main

import (
	"math"
	"testing"
)

// tick advances an NPC by n frames of 1/60 s with no collision map.
func tick(n *NPC, frames int, players []playerPos) {
	for i := 0; i < frames; i++ {
		n.update(1.0/60.0, nil, players)
	}
}

func distTo(n *NPC, x, y float64) float64 {
	dx, dy := n.state.X-x, n.state.Y-y
	return math.Sqrt(dx*dx + dy*dy)
}

func newTestNPC(name string, x, y float64, npcType int, b Behaviour) *NPC {
	n := newNPC("t_"+name, name, x, y, npcType)
	n.worldW, n.worldH = 4000, 4000
	n.SetBehaviour(b)
	return n
}

func TestBehaviourParsingAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Behaviour
	}{
		{"wander", BehaviourWander},
		{"roam", BehaviourRoam},
		{"patrol", BehaviourPatrol},
		{"passive", BehaviourPassive},
		{"aggressive", BehaviourAggressive},
		{"static", BehaviourStatic},
		{"nonsense", BehaviourWander},
		{"", BehaviourWander},
	} {
		if got := ParseBehaviour(tc.in); got != tc.want {
			t.Errorf("ParseBehaviour(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	// Existing NPC types must keep behaving as they did before behaviours existed.
	for _, tc := range []struct {
		npcType int
		want    Behaviour
	}{
		{NPCTypeAggressive, BehaviourAggressive},
		{NPCTypeSpawnedEnemy, BehaviourAggressive},
		{NPCTypePassive, BehaviourPassive},
		{NPCTypeGuard, BehaviourPatrol},
		{NPCTypeVillager, BehaviourWander},
		{NPCTypeHorse, BehaviourWander},
	} {
		if got := defaultBehaviourFor(tc.npcType); got != tc.want {
			t.Errorf("defaultBehaviourFor(%d) = %v, want %v", tc.npcType, got, tc.want)
		}
	}
}

// A guard asked to patrol without an explicit route gets a generated one.
func TestPatrolGetsDefaultRoute(t *testing.T) {
	n := newTestNPC("guard", 500, 500, NPCTypeGuard, BehaviourPatrol)
	if len(n.waypoints) == 0 {
		t.Fatal("patrolling NPC has no waypoints")
	}
	for _, wp := range n.waypoints {
		if distTo(&NPC{state: NPCState{X: 500, Y: 500}}, wp.X, wp.Y) > patrolDefaultRadius*2 {
			t.Errorf("generated waypoint %v is unexpectedly far from home", wp)
		}
	}
}

// A patrolling NPC must visit every waypoint and loop back round.
// Asserting on the index at two arbitrary moments is not enough: a short route
// can be back on the same index after a full cycle.
func TestPatrolVisitsEveryWaypointAndLoops(t *testing.T) {
	route := []vec2{{600, 500}, {600, 600}, {500, 600}}
	n := newTestNPC("guard", 500, 500, NPCTypeGuard, BehaviourPatrol)
	n.SetWaypoints(route)
	n.speed = 200

	visited := make(map[int]int)
	prev := n.wpIndex
	for i := 0; i < 3600; i++ { // 60 s: several full loops
		n.update(1.0/60.0, nil, nil)
		if n.wpIndex != prev {
			visited[prev]++ // completing a waypoint advances the index
			prev = n.wpIndex
		}
	}

	for i := range route {
		if visited[i] == 0 {
			t.Errorf("waypoint %d was never reached (visits: %v)", i, visited)
		}
	}
	if visited[0] < 2 {
		t.Errorf("route did not loop: waypoint 0 completed %d time(s), want ≥2", visited[0])
	}

	// It must actually walk the route, not sit at its starting corner.
	if distTo(n, 500, 500) < 1 && !n.state.Moving {
		t.Error("patrolling NPC never left its start position")
	}
}

// An empty route reverts the NPC to wandering rather than freezing it.
func TestEmptyWaypointsRevertsToWander(t *testing.T) {
	n := newTestNPC("guard", 500, 500, NPCTypeGuard, BehaviourPatrol)
	n.SetWaypoints(nil)
	if n.behaviour != BehaviourWander {
		t.Errorf("behaviour = %v, want wander", n.behaviour)
	}
}

func TestStaticNPCNeverMoves(t *testing.T) {
	n := newTestNPC("merchant", 500, 500, NPCTypeMerchant, BehaviourStatic)
	tick(n, 600, nil)
	if n.state.X != 500 || n.state.Y != 500 {
		t.Errorf("static NPC moved to (%.1f,%.1f)", n.state.X, n.state.Y)
	}
	if n.state.Moving {
		t.Error("static NPC reports Moving = true")
	}
}

// An aggressive NPC dragged beyond its leash must give up and walk home instead
// of taking up residence wherever the player left it.
func TestAggroLeashReturnsHome(t *testing.T) {
	n := newTestNPC("slime", 500, 500, NPCTypeAggressive, BehaviourAggressive)
	n.speed = 300

	// A player leads it away, staying just in front of it.
	px, py := 500.0, 500.0
	for i := 0; i < 600; i++ {
		px += 3 // player walks away faster than nothing, NPC follows
		n.update(1.0/60.0, nil, []playerPos{{id: "p1", x: px, y: py, alive: true}})
		if n.state2 == aiReturning {
			break
		}
	}
	if n.state2 != aiReturning {
		t.Fatalf("NPC never leashed: state=%v, %.0f px from home", n.state2, distTo(n, 500, 500))
	}
	if n.aggroTarget != "" {
		t.Error("leashed NPC still holds an aggro target")
	}

	// With the player gone it should make it back home and resume normal AI.
	for i := 0; i < 3000 && n.state2 == aiReturning; i++ {
		n.update(1.0/60.0, nil, nil)
	}
	if n.state2 != aiNormal {
		t.Errorf("NPC never finished returning home (%.0f px away)", distTo(n, 500, 500))
	}
	if d := distTo(n, 500, 500); d > returnHomeDoneDist+2 {
		t.Errorf("NPC stopped %.0f px from home, want within %.0f", d, returnHomeDoneDist)
	}
}

// Losing sight of the target sends the NPC home rather than leaving it wandering
// wherever the chase ended.
func TestChaseLostReturnsHome(t *testing.T) {
	n := newTestNPC("slime", 500, 500, NPCTypeAggressive, BehaviourAggressive)
	n.speed = 150

	// Player in range: the NPC starts chasing.
	tick(n, 30, []playerPos{{id: "p1", x: 560, y: 500, alive: true}})
	if n.state2 != aiChasing {
		t.Fatalf("NPC did not start chasing a player 60 px away (state=%v)", n.state2)
	}

	// Player vanishes.
	n.update(1.0/60.0, nil, nil)
	if n.state2 != aiReturning {
		t.Errorf("state after losing target = %v, want returning", n.state2)
	}
}

// damageNPC must provoke the NPC, and the alert must survive until the hub
// consumes it — it is set between ticks, so update() must not clear it.
func TestDamageProvokesAndAlertSurvivesTick(t *testing.T) {
	h := newTestHub()
	n := newTestNPC("slime", 500, 500, NPCTypeAggressive, BehaviourAggressive)
	ally := newTestNPC("gobelin", 560, 500, NPCTypeAggressive, BehaviourAggressive)
	h.npcs = []*NPC{n, ally}

	if hp, _ := h.damageNPC(n.state.ID, defaultMap, 1); hp < 0 {
		t.Fatal("damageNPC reported immunity on a fresh NPC")
	}
	if !n.raisedAlert {
		t.Fatal("damage did not raise an alert")
	}

	// One AI tick happens before the hub's propagation pass; the flag must persist.
	n.update(1.0/60.0, nil, nil)
	if !n.raisedAlert {
		t.Fatal("update() cleared the alert before the hub could consume it")
	}

	h.propagateAlerts()
	if n.raisedAlert {
		t.Error("propagateAlerts did not consume the alert")
	}
	if !ally.alertedByAlly {
		t.Error("nearby ally was not alerted")
	}
}

// Alerts are scoped by radius, map instance and liveness. Allies here are the
// shouter's own kind so that only those three rules are under test.
func TestAlertPropagationScope(t *testing.T) {
	h := newTestHub()
	shouter := newTestNPC("shouter", 1000, 1000, NPCTypeAggressive, BehaviourAggressive)
	near := newTestNPC("near", 1000+alertRadius-20, 1000, NPCTypeAggressive, BehaviourAggressive)
	far := newTestNPC("far", 1000+alertRadius+200, 1000, NPCTypeAggressive, BehaviourAggressive)
	passive := newTestNPC("passive", 1010, 1000, NPCTypePassive, BehaviourPassive)
	otherMapAlly := newTestNPC("elsewhere", 1010, 1000, NPCTypeAggressive, BehaviourAggressive)
	otherMapAlly.mapID = otherMap
	dead := newTestNPC("dead", 1010, 1000, NPCTypeAggressive, BehaviourAggressive)
	dead.combat.alive = false

	h.npcs = []*NPC{shouter, near, far, passive, otherMapAlly, dead}
	shouter.raisedAlert = true
	h.propagateAlerts()

	if !near.alertedByAlly {
		t.Error("ally within alertRadius was not alerted")
	}
	if far.alertedByAlly {
		t.Error("ally beyond alertRadius was alerted")
	}
	if passive.alertedByAlly {
		t.Error("passive NPC answered an alert")
	}
	if otherMapAlly.alertedByAlly {
		t.Error("alert crossed a map instance")
	}
	if dead.alertedByAlly {
		t.Error("dead NPC was alerted")
	}
}

// An alerted guard chases even though patrolling is its baseline behaviour.
func TestAlertedGuardChases(t *testing.T) {
	guard := newTestNPC("guard", 500, 500, NPCTypeGuard, BehaviourPatrol)
	guard.speed = 200
	guard.alertedByAlly = true

	player := []playerPos{{id: "p1", x: 600, y: 500, alive: true}}
	startDist := distTo(guard, 600, 500)
	tick(guard, 60, player)

	if guard.state2 != aiChasing {
		t.Fatalf("alerted guard state = %v, want chasing", guard.state2)
	}
	if d := distTo(guard, 600, 500); d >= startDist {
		t.Errorf("alerted guard did not close in: %.0f px → %.0f px", startDist, d)
	}
}

// The alert cooldown stops a long fight from re-alerting every single tick.
func TestAlertCooldown(t *testing.T) {
	n := newTestNPC("slime", 500, 500, NPCTypeAggressive, BehaviourAggressive)
	n.raiseAlert()
	if !n.raisedAlert {
		t.Fatal("first raiseAlert did not set the flag")
	}
	n.raisedAlert = false
	n.raiseAlert()
	if n.raisedAlert {
		t.Error("raiseAlert fired again while on cooldown")
	}
}

// A roaming NPC keeps moving and stays within roamRadius of home.
func TestRoamStaysWithinRadius(t *testing.T) {
	n := newTestNPC("traveller", 2000, 2000, NPCTypeTraveler, BehaviourRoam)
	n.speed = 200

	moved := false
	for i := 0; i < 3600; i++ {
		prevX, prevY := n.state.X, n.state.Y
		n.update(1.0/60.0, nil, nil)
		if n.state.X != prevX || n.state.Y != prevY {
			moved = true
		}
		if d := distTo(n, 2000, 2000); d > roamRadius+100 {
			t.Fatalf("roamer strayed %.0f px from home, limit ~%.0f", d, roamRadius)
		}
	}
	if !moved {
		t.Error("roaming NPC never moved")
	}
}

// moveToward must not overshoot its target and oscillate around it.
func TestMoveTowardDoesNotOvershoot(t *testing.T) {
	n := newTestNPC("x", 0, 0, NPCTypeVillager, BehaviourWander)
	n.speed = 10000 // absurdly fast: one step would fly past the target
	arrived := n.moveToward(1.0/60.0, nil, 5, 0, n.speed, 1.0)
	if !arrived && (n.state.X > 5.001 || n.state.X < 0) {
		t.Errorf("overshot: X = %.4f, target 5", n.state.X)
	}
}

// wallCollider blocks everything at or beyond blockX on the X axis.
type wallCollider struct{ blockX float64 }

func (w wallCollider) IsBlocked(x, y, bw, bh float64) bool { return x+bw >= w.blockX }
func (w wallCollider) IsFreePoint(x, y float64) bool       { return x < w.blockX }
func (w wallCollider) Bounds() (float64, float64)          { return 4000, 4000 }

// Regression: an NPC walking a perfectly axis-aligned leg into a wall must be
// recognised as blocked.
//
// The bug: with dy == 0 the candidate Y equals the current Y, so the collision
// test for the Y axis trivially passed on a position the NPC already occupied.
// That counted as movement, reset the blocked timers every tick, and left the
// NPC frozen against the wall forever while still reporting Moving = true —
// which is exactly how squarePatrol legs are shaped.
func TestBlockedDetectedOnAxisAlignedTarget(t *testing.T) {
	coll := wallCollider{blockX: 1000}
	n := newTestNPC("walker", 900, 500, NPCTypeVillager, BehaviourWander)
	n.speed = 120

	// Target due east, past the wall: dy is exactly 0.
	for i := 0; i < 120; i++ {
		n.moveToward(1.0/60.0, coll, 2000, 500, n.speed, 4.0)
	}

	if n.blockedTime < 0.5 {
		t.Fatalf("blockedTime only reached %.2f while walled in on an axis-aligned path", n.blockedTime)
	}
	// It must not have tunnelled through the wall. Sideways drift in Y is fine —
	// that is the detour logic doing its job.
	if n.state.X+npcW >= coll.blockX {
		t.Errorf("NPC crossed the wall: X = %.2f", n.state.X)
	}
}

// The same situation must make a patrolling NPC skip the unreachable waypoint
// rather than stand against the wall for the rest of the session.
func TestPatrolSkipsUnreachableWaypoint(t *testing.T) {
	coll := wallCollider{blockX: 1000}
	n := newTestNPC("guard", 900, 500, NPCTypeGuard, BehaviourPatrol)
	n.speed = 120
	// First leg is due east into the wall; second is reachable.
	n.SetWaypoints([]vec2{{2000, 500}, {800, 500}})

	for i := 0; i < 600 && n.wpIndex == 0; i++ {
		n.update(1.0/60.0, coll, nil)
	}
	if n.wpIndex == 0 {
		t.Fatalf("guard never gave up on the unreachable waypoint (blocked=%.2f)", n.blockedTime)
	}
}

// An NPC meeting an obstacle must steer around it and reach its destination,
// not shuffle against it until it happens to slip past.
func TestSteersAroundObstacle(t *testing.T) {
	// A wall segment across the direct path, with clear space above and below.
	coll := &obstacleCollider{obstacles: []aabb{{x: 400, y: 400, w: 60, h: 200}}}

	n := newTestNPC("walker", 100, 500, NPCTypeVillager, BehaviourWander)
	n.speed = 200

	reached := false
	for i := 0; i < 1800; i++ { // 30 s is far more than the ~5 s needed
		if n.moveToward(1.0/60.0, coll, 900, 500, n.speed, 8.0) {
			reached = true
			break
		}
	}
	if !reached {
		t.Fatalf("never got past the obstacle: at (%.0f,%.0f), blocked=%.1f",
			n.state.X, n.state.Y, n.blockedTime)
	}
	// It must have gone around, not through.
	if coll.IsBlocked(n.state.X, n.state.Y, npcW, npcH) {
		t.Error("NPC ended up inside the obstacle")
	}
}

// While negotiating an obstacle the NPC must keep to one side rather than
// alternating each tick, which is what produced the visible shuffle.
func TestSteeringCommitsToOneSide(t *testing.T) {
	coll := &obstacleCollider{obstacles: []aabb{{x: 400, y: 300, w: 60, h: 400}}}
	n := newTestNPC("walker", 360, 500, NPCTypeVillager, BehaviourWander)
	n.speed = 120

	// Walk until it actually reaches the obstacle and steering engages.
	for i := 0; i < 300 && n.steerSide == 0; i++ {
		n.moveToward(1.0/60.0, coll, 900, 500, n.speed, 8.0)
	}
	side := n.steerSide
	if side == 0 {
		t.Fatal("steering never engaged against an obstacle")
	}
	flips := 0
	for i := 0; i < 120; i++ {
		n.moveToward(1.0/60.0, coll, 900, 500, n.speed, 8.0)
		if n.steerSide != 0 && n.steerSide != side {
			flips++
			side = n.steerSide
		}
	}
	if flips > 2 {
		t.Errorf("steering side flipped %d times in 2 s, expected it to commit", flips)
	}
}

// A steering NPC must face the direction it actually moves, not the blocked
// direction it wanted — the sprite would otherwise walk sideways.
func TestFacingFollowsActualMovement(t *testing.T) {
	// Wall to the east: the NPC wants to go right but must go up or down.
	coll := &obstacleCollider{obstacles: []aabb{{x: 400, y: 0, w: 60, h: 1000}}}
	n := newTestNPC("walker", 360, 500, NPCTypeVillager, BehaviourWander)
	n.speed = 120

	beforeY := n.state.Y
	for i := 0; i < 30; i++ {
		n.moveToward(1.0/60.0, coll, 900, 500, n.speed, 8.0)
	}
	if n.state.Y == beforeY {
		t.Fatal("NPC did not deflect along the wall")
	}
	wantDir := 2 // down
	if n.state.Y < beforeY {
		wantDir = 0 // up
	}
	if n.state.Dir != wantDir {
		t.Errorf("Dir = %d while moving %s", n.state.Dir,
			map[int]string{0: "up", 2: "down"}[wantDir])
	}
}

// Passive animals must run away smoothly, not vibrate. Fleeing toward a point
// computed from the animal's own position made the target recede every tick, so
// the stall detector never saw progress and shook the animal in place.
func TestPassiveFleesSmoothly(t *testing.T) {
	n := newTestNPC("chicken", 500, 500, NPCTypePassive, BehaviourPassive)
	n.speed = 120

	player := []playerPos{{id: "p1", x: 480, y: 500, alive: true}}
	path := 0.0
	prevX, prevY := n.state.X, n.state.Y
	for i := 0; i < 180; i++ { // 3 s
		n.update(1.0/60.0, nil, player)
		path += math.Hypot(n.state.X-prevX, n.state.Y-prevY)
		prevX, prevY = n.state.X, n.state.Y
	}

	if got := distTo(n, 480, 500); got < 100 {
		t.Errorf("only got %.0f px from the player in 3 s", got)
	}
	// Path length close to distance covered means it ran, rather than jittering.
	straight := distTo(n, 500, 500)
	if path > straight*2.5 {
		t.Errorf("path %.0f px for %.0f px of progress — movement is not smooth", path, straight)
	}
	if n.blockedTime > 0.5 {
		t.Errorf("fleeing animal reported blocked for %.1fs in open ground", n.blockedTime)
	}
}

// Hysteresis: an animal must not flip between fleeing and wandering at the
// threshold, which looked like flickering.
func TestPassiveFleeHysteresis(t *testing.T) {
	n := newTestNPC("deer", 500, 500, NPCTypePassive, BehaviourPassive)
	n.speed = 120

	// Inside the trigger range: starts fleeing.
	n.update(1.0/60.0, nil, []playerPos{{id: "p1", x: 500 + passiveFleeRange - 10, y: 500, alive: true}})
	if !n.fleeing {
		t.Fatal("did not start fleeing inside passiveFleeRange")
	}
	// Just outside the trigger range but inside the stop range: keeps fleeing.
	n.update(1.0/60.0, nil, []playerPos{{id: "p1", x: 500 + passiveFleeRange + 20, y: 500, alive: true}})
	if !n.fleeing {
		t.Error("stopped fleeing between the two thresholds — this is the flip-flop")
	}
	// Well beyond the stop range: calms down.
	n.update(1.0/60.0, nil, []playerPos{{id: "p1", x: 500 + passiveFleeStop + 50, y: 500, alive: true}})
	if n.fleeing {
		t.Error("still fleeing beyond passiveFleeStop")
	}
}

// The server reports the velocity an NPC actually achieved, because the client
// dead-reckons with it. A guess based on facing direction cannot be right: NPCs
// move diagonally at randomised speeds.
func TestNPCReportsMeasuredVelocity(t *testing.T) {
	n := newTestNPC("walker", 500, 500, NPCTypeVillager, BehaviourWander)
	n.speed = 120
	n.targetX, n.targetY = 900, 900 // diagonal, so both axes are non-zero
	n.timer = 99                    // do not re-target mid-test

	const dt = 1.0 / 60.0
	beforeX, beforeY := n.state.X, n.state.Y
	n.update(dt, nil, nil)

	wantVX := (n.state.X - beforeX) / dt
	wantVY := (n.state.Y - beforeY) / dt
	if math.Abs(n.state.VX-wantVX) > 1e-6 || math.Abs(n.state.VY-wantVY) > 1e-6 {
		t.Errorf("reported velocity (%.3f,%.3f), actual (%.3f,%.3f)",
			n.state.VX, n.state.VY, wantVX, wantVY)
	}
	if n.state.VX <= 0 || n.state.VY <= 0 {
		t.Errorf("expected movement on both axes, got (%.3f,%.3f)", n.state.VX, n.state.VY)
	}
	// Magnitude must match the configured speed, which is what a per-type guess
	// would have got wrong.
	if got := math.Hypot(n.state.VX, n.state.VY); math.Abs(got-n.speed) > 1.0 {
		t.Errorf("velocity magnitude %.1f px/s, want ~%.1f", got, n.speed)
	}
}

// A teleport must report zero velocity, or clients would predict off-world.
func TestTeleportReportsZeroVelocity(t *testing.T) {
	n := newTestNPC("mob", 500, 500, NPCTypeAggressive, BehaviourAggressive)
	n.combat.HP = 1
	n.combat.noRespawn = false
	// Kill it, then run the respawn timer out: respawn snaps it back home from
	// wherever it died.
	n.state.X, n.state.Y = 3000, 3000
	n.combat.Damage(1)
	for i := 0; i < 60*(int(npcRespawnTime)+1); i++ {
		n.update(1.0/60.0, nil, nil)
	}
	if n.state.X != n.homeX || n.state.Y != n.homeY {
		t.Fatalf("NPC did not respawn home: (%.0f,%.0f) vs (%.0f,%.0f)",
			n.state.X, n.state.Y, n.homeX, n.homeY)
	}
	if n.state.VX != 0 || n.state.VY != 0 {
		t.Errorf("respawn reported velocity (%.1f,%.1f), want (0,0)", n.state.VX, n.state.VY)
	}
}

// Only guards and monsters answer alerts or fight back. A horse or villager
// turning on the player because a nearby monster shouted looked like a bug.
func TestOnlyFightersAnswerAlerts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		npcType int
		b       Behaviour
		want    bool
	}{
		{"horse", NPCTypeHorse, BehaviourWander, false},
		{"villager", NPCTypeVillager, BehaviourWander, false},
		{"merchant", NPCTypeMerchant, BehaviourStatic, false},
		{"chicken", NPCTypePassive, BehaviourPassive, false},
		{"guard", NPCTypeGuard, BehaviourPatrol, true},
		{"slime", NPCTypeAggressive, BehaviourAggressive, true},
		{"roaming bat", NPCTypeAggressive, BehaviourRoam, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newTestNPC(tc.name, 500, 500, tc.npcType, tc.b)
			if got := n.canFight(); got != tc.want {
				t.Fatalf("canFight() = %v, want %v", got, tc.want)
			}

			n.provoke()
			chasing := n.state2 == aiChasing
			if chasing != tc.want {
				t.Errorf("provoked → chasing = %v, want %v", chasing, tc.want)
			}
			// Everything may still call for help, even a rabbit.
			if !n.raisedAlert {
				t.Error("provoked NPC did not raise an alert")
			}
		})
	}
}

// A bystander must never be pulled into a fight, even by an ally of its own type
// area. The shouter here is a guard so the other guard does answer — the point is
// that the horse standing between them does not.
func TestAlertDoesNotArmBystanders(t *testing.T) {
	h := newTestHub()
	shouter := newTestNPC("guard", 500, 500, NPCTypeGuard, BehaviourPatrol)
	horse := newTestNPC("horse", 540, 500, NPCTypeHorse, BehaviourWander)
	villager := newTestNPC("villager", 550, 500, NPCTypeVillager, BehaviourWander)
	ally := newTestNPC("guard2", 560, 500, NPCTypeGuard, BehaviourPatrol)
	h.npcs = []*NPC{shouter, horse, villager, ally}

	shouter.raisedAlert = true
	h.propagateAlerts()

	if horse.alertedByAlly {
		t.Error("horse was called into a fight")
	}
	if villager.alertedByAlly {
		t.Error("villager was called into a fight")
	}
	if !ally.alertedByAlly {
		t.Error("fellow guard ignored the alert")
	}
}

// Alerts stay within a side: a monster shouting must not turn the town guard
// against the player, though hitting the guard directly still does.
func TestAlertsStayWithinASide(t *testing.T) {
	h := newTestHub()
	monster := newTestNPC("slime", 500, 500, NPCTypeAggressive, BehaviourAggressive)
	otherMonster := newTestNPC("gobelin", 540, 500, NPCTypeAggressive, BehaviourAggressive)
	guard := newTestNPC("guard", 560, 500, NPCTypeGuard, BehaviourPatrol)
	h.npcs = []*NPC{monster, otherMonster, guard}

	monster.raisedAlert = true
	h.propagateAlerts()
	if !otherMonster.alertedByAlly {
		t.Error("monster did not rally another monster")
	}
	if guard.alertedByAlly {
		t.Error("a monster's alert recruited the guard")
	}

	// A guard's own alert rallies guards.
	guard2 := newTestNPC("guard2", 580, 500, NPCTypeGuard, BehaviourPatrol)
	h.npcs = append(h.npcs, guard2)
	guard.raisedAlert = true
	h.propagateAlerts()
	if !guard2.alertedByAlly {
		t.Error("guard did not rally another guard")
	}

	// Hitting the guard directly still makes it fight.
	g3 := newTestNPC("guard3", 500, 500, NPCTypeGuard, BehaviourPatrol)
	g3.provoke()
	if g3.state2 != aiChasing {
		t.Error("a directly provoked guard did not fight back")
	}
}

// A dismounted horse must stay where it was left. Keeping its pre-mount home and
// wander target made it turn round and trek back to where it first stood the
// instant the rider stepped off.
func TestDismountedHorseStaysWhereLeft(t *testing.T) {
	h := newTestHub()
	horse := newTestNPC("horse", 500, 500, NPCTypeHorse, BehaviourWander)
	horse.speed = 80
	h.npcs = []*NPC{horse}

	if !h.mountNPC(horse.state.ID, defaultMap, "player_1") {
		t.Fatal("could not mount the horse")
	}

	// Ride it a long way: the rider's move messages drag the horse along.
	h.mu.Lock()
	h.updateHorsePos("player_1", 1400, 900)
	h.mu.Unlock()

	h.dismountNPC("player_1")
	if horse.mountedBy != "" || horse.state.MountedBy != "" {
		t.Error("dismount left the horse marked as ridden")
	}
	if horse.homeX != 1400 || horse.homeY != 900 {
		t.Errorf("home stayed at (%.0f,%.0f), want the dismount spot (1400,900)",
			horse.homeX, horse.homeY)
	}

	// Let it graze for a while: it must not head back toward its old home.
	for i := 0; i < 600; i++ {
		horse.update(1.0/60.0, nil, nil)
	}
	if d := distTo(horse, 1400, 900); d > 260 {
		t.Errorf("horse wandered %.0f px from where it was left — it is going home", d)
	}
	if distTo(horse, 500, 500) < 400 {
		t.Error("horse headed back to its original spot")
	}
}
