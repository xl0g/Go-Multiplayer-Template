package main

import (
	"math"
	"time"
)

// respawnDelay is how long after collection before a gralat reappears.
const respawnDelay = 45 * time.Second

// gralatReach is how far a player may be from a pickup and still collect it.
// Generous enough to absorb client interpolation and latency, tight enough that
// a client cannot claim coins it never walked over.
const gralatReach = 64.0

// gralatSpawnDefs defines the values of the gralat pickups and where they sit
// relative to the configured spawn point (Hub.spawnX/spawnY), in pixels.
// Offsets rather than absolute coordinates: the previous absolute values were
// authored for the 1120×1120 TMX world and put every coin tens of thousands of
// pixels away from spawn on any larger map.
var gralatSpawnDefs = []struct {
	id     string
	dx, dy float64
	value  int
}{
	{"g0", -320, -120, 1},
	{"g1", 80, -80, 5},
	{"g2", 420, -20, 1},
	{"g3", -230, 280, 30},
	{"g4", 240, 180, 5},
	{"g5", 520, -140, 1},
	{"g6", -80, 480, 100},
	{"g7", 170, 430, 5},
}

// findFreePos searches outward from (x,y) in a spiral for a non-blocked tile.
// worldW/worldH are the actual world bounds (may exceed the TMX mapWidth/mapHeight).
func findFreePos(cm WorldCollider, x, y, worldW, worldH float64) (float64, float64) {
	step := 16.0
	for radius := step; radius <= 200; radius += step {
		for angle := 0.0; angle < 2*math.Pi; angle += math.Pi / 8 {
			nx := x + math.Cos(angle)*radius
			ny := y + math.Sin(angle)*radius
			if nx < 0 || ny < 0 || nx >= worldW || ny >= worldH {
				continue
			}
			if cm.IsFreePoint(nx+8, ny+8) {
				return nx, ny
			}
		}
	}
	return x, y
}
