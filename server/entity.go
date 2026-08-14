package main

// Combat timing constants shared by NPCs and players.
const (
	npcRespawnTime  = 15.0 // seconds before a dead NPC respawns
	hitInvincibleDt = 0.5  // seconds of invulnerability after being hit
)

// CombatEntity holds the shared HP, damage, and respawn state for any living actor.
// Embed it in NPC and Client so both use identical combat rules.
type CombatEntity struct {
	HP, MaxHP  int
	alive      bool
	hitCD      float64 // invulnerability seconds remaining
	atkCD      float64 // seconds until next attack
	deathTimer float64 // counts down to respawn (0 once alive or noRespawn)
	noRespawn  bool    // if true, no automatic respawn after death
}

// newCombat creates a CombatEntity with full HP and alive=true.
// MaxHP == 0 means immortal (see Damage) — such an entity is still alive,
// otherwise it would be born dead and get filtered out of state broadcasts.
func newCombat(maxHP int) CombatEntity {
	return CombatEntity{HP: maxHP, MaxHP: maxHP, alive: true}
}

// Damage applies dmg to the entity.
// Returns (newHP, killed). Returns (-1, false) when immune:
// already dead, within hit-invulnerability window, or immortal (MaxHP==0).
func (e *CombatEntity) Damage(dmg int) (int, bool) {
	if !e.alive || e.hitCD > 0 || e.MaxHP == 0 {
		return -1, false
	}
	e.HP -= dmg
	e.hitCD = hitInvincibleDt
	if e.HP <= 0 {
		e.HP = 0
		e.alive = false
		if !e.noRespawn {
			e.deathTimer = npcRespawnTime
		}
		return 0, true
	}
	return e.HP, false
}

// Tick advances combat cooldowns and automatic respawn.
// Returns true on the tick the entity respawns.
func (e *CombatEntity) Tick(dt float64) (respawned bool) {
	if e.hitCD > 0 {
		e.hitCD -= dt
		if e.hitCD < 0 {
			e.hitCD = 0
		}
	}
	if e.atkCD > 0 {
		e.atkCD -= dt
		if e.atkCD < 0 {
			e.atkCD = 0
		}
	}
	if !e.alive && !e.noRespawn && e.deathTimer > 0 {
		e.deathTimer -= dt
		if e.deathTimer <= 0 {
			e.alive = true
			e.HP = e.MaxHP
			return true
		}
	}
	return false
}

// IsAlive reports whether the entity is alive.
func (e *CombatEntity) IsAlive() bool { return e.alive }

// CanAttack reports whether the attack cooldown has expired.
func (e *CombatEntity) CanAttack() bool { return e.atkCD <= 0 }

// RecentlyDied returns true if the entity just died and is still in the death-
// animation window (last 2 s of respawn countdown). Used to keep dead NPCs
// visible a bit longer before they vanish from clients.
func (e *CombatEntity) RecentlyDied() bool {
	return !e.alive && !e.noRespawn && e.deathTimer > npcRespawnTime-2.0
}
