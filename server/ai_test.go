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

// Being hit provokes a reaction even from a peaceful NPC's neighbours.
func TestProvokeMakesNonPassiveFightBack(t *testing.T) {
	villager := newTestNPC("villager", 500, 500, NPCTypeVillager, BehaviourWander)
	villager.provoke()
	if villager.state2 != aiChasing {
		t.Errorf("provoked villager state = %v, want chasing", villager.state2)
	}
	if !villager.raisedAlert {
		t.Error("provoked NPC did not raise an alert")
	}

	// A passive animal must flee, never turn and fight.
	rabbit := newTestNPC("rabbit", 500, 500, NPCTypePassive, BehaviourPassive)
	rabbit.provoke()
	if rabbit.state2 == aiChasing {
		t.Error("provoked passive NPC switched to chasing")
	}
	if !rabbit.raisedAlert {
		t.Error("provoked passive NPC should still call for help")
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

func TestAlertPropagationScope(t *testing.T) {
	h := newTestHub()
	shouter := newTestNPC("shouter", 1000, 1000, NPCTypeAggressive, BehaviourAggressive)
	near := newTestNPC("near", 1000+alertRadius-20, 1000, NPCTypeGuard, BehaviourPatrol)
	far := newTestNPC("far", 1000+alertRadius+200, 1000, NPCTypeGuard, BehaviourPatrol)
	passive := newTestNPC("passive", 1010, 1000, NPCTypePassive, BehaviourPassive)
	otherMapAlly := newTestNPC("elsewhere", 1010, 1000, NPCTypeGuard, BehaviourPatrol)
	otherMapAlly.mapID = otherMap
	dead := newTestNPC("dead", 1010, 1000, NPCTypeGuard, BehaviourPatrol)
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

// An NPC pressed against a wall must keep one detour direction instead of
// re-rolling a random angle every tick and vibrating in place.
func TestUnstickCommitsToADirection(t *testing.T) {
	n := newTestNPC("x", 100, 100, NPCTypeVillager, BehaviourWander)
	n.unstick(0.3, nil, 1, 0, 2) // past the 0.25 s threshold
	if !n.stuckAngleSet {
		t.Fatal("unstick did not commit to a detour angle")
	}
	angle := n.stuckAngle
	n.stuckTimer = 0.3
	n.unstick(0.1, nil, 1, 0, 2)
	if n.stuckAngleSet && n.stuckAngle != angle {
		t.Errorf("detour angle changed mid-detour: %.3f → %.3f", angle, n.stuckAngle)
	}
}
