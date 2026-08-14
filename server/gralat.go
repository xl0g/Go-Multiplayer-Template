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

// gralatSpawnDefs defines the fixed world positions and values of gralat pickups.
// Positions are in pixels for the 1120×1120 world (70×70 tiles at 16 px each).
var gralatSpawnDefs = []struct {
	id    string
	x, y  float64
	value int
}{
	{"g0", 160, 200, 1},
	{"g1", 560, 240, 5},
	{"g2", 900, 300, 1},
	{"g3", 250, 600, 30},
	{"g4", 720, 500, 5},
	{"g5", 1000, 180, 1},
	{"g6", 400, 800, 100},
	{"g7", 650, 750, 5},
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
