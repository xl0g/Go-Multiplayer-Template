package main

import "testing"

// Regression: dead reckoning must clamp predicted positions against the world
// actually loaded, not the compile-time TMX constants.
//
// With the constants (1120 px) every remote entity on a larger world had its
// predicted X/Y yanked back to ~1088 each frame, which exceeded the 200 px
// "large desync" threshold and snapped the sprite to the corner — NPCs and other
// players visibly teleported back and forth at 60 Hz.
func TestDeadReckoningClampsToActiveWorld(t *testing.T) {
	oldW, oldH := activeWorldW, activeWorldH
	defer func() { activeWorldW, activeWorldH = oldW, oldH }()

	// A GMAP-sized world, like the configured classiciphone.gmap.
	activeWorldW, activeWorldH = 26624, 23552

	c := &Character{
		X: 15900, Y: 12000,
		TargetX: 15900, TargetY: 12000,
		Moving: true,
		velX:   100, velY: 50,
	}
	c.applyRemoteMotion(1.0 / 60.0)

	if c.TargetX < 15900 {
		t.Errorf("TargetX moved backwards to %.1f — clamped against a stale world size", c.TargetX)
	}
	if c.TargetY < 12000 {
		t.Errorf("TargetY moved backwards to %.1f", c.TargetY)
	}
	// The sprite must follow smoothly, not snap.
	if d := (c.TargetX - c.X) * (c.TargetX - c.X); d > 200*200 {
		t.Errorf("desync of %.0f px would trigger the snap path", c.TargetX-c.X)
	}
}

// The clamp must still keep entities inside the world.
func TestDeadReckoningStillClamps(t *testing.T) {
	oldW, oldH := activeWorldW, activeWorldH
	defer func() { activeWorldW, activeWorldH = oldW, oldH }()
	activeWorldW, activeWorldH = 1120, 1120

	c := &Character{
		X: 1000, Y: 1000, TargetX: 1000, TargetY: 1000,
		Moving: true, velX: 100000, velY: 100000,
	}
	c.applyRemoteMotion(1.0 / 60.0)

	if maxX := activeWorldW - float64(frameW); c.TargetX > maxX {
		t.Errorf("TargetX = %.1f exceeds world max %.1f", c.TargetX, maxX)
	}
	if maxY := activeWorldH - float64(frameH); c.TargetY > maxY {
		t.Errorf("TargetY = %.1f exceeds world max %.1f", c.TargetY, maxY)
	}
}
