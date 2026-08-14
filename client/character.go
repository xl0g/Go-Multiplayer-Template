package main

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"path/filepath"
	"sync"

	"github.com/hajimehoshi/ebiten/v2"
	"golang.org/x/image/font/basicfont"
)

// ──────────────────────────────────────────────────────────────
// Constants
// ──────────────────────────────────────────────────────────────

const (
	frameW = 32
	frameH = 32

	// NPC type for horse (must match server constant)
	NPCTypeHorse         = 5
	NPCTypeAggressive    = 6
	NPCTypePassive       = 7
	NPCTypeSpawnedEnemy  = 8
)

// Gani animation states
const (
	AnimIdle          = "idle"
	AnimWalk          = "walk"
	AnimSword         = "sword"
	AnimRide          = "ride"
	AnimSit           = "sit"
	AnimGrab          = "grab"           // grab.gani — hold A to grab a wall
	AnimJuggle        = "juggle"         // juggle.gani (original)
	AnimClassicJuggle = "classic_juggle" // classic_new_juggle.gani (inventory item)
	AnimPompoms       = "pompoms"        // ci_pompoms.gani (inventory item)
	AnimHatTrick      = "hattrick"
	AnimPush          = "push"
	AnimDead          = "dead"
	AnimHurt          = "hurt"
	AnimSwim          = "swim" // swim.gani — player is on a water tile
	AnimLava          = "lava" // lava.gani — player is on a lava tile
)

// ganiFile maps animation state → .gani filename.
var ganiFile = map[string]string{
	AnimIdle:  "idle.gani",
	AnimWalk:  "walk.gani",
	AnimSword: "sword.gani",
	AnimRide:  "mount.gani",
	AnimSwim:  "swim.gani",
	AnimLava:  "lava.gani",
}

// npcTints maps NPCType → rendering tint.
var npcTints = []color.RGBA{
	{160, 230, 160, 255}, // 0 villager
	{255, 210, 100, 255}, // 1 merchant
	{140, 160, 255, 255}, // 2 guard
	{255, 160, 200, 255}, // 3 traveler
	{210, 170, 120, 255}, // 4 farmer
	{220, 190, 130, 255}, // 5 horse
	{255, 80, 80, 255},   // 6 aggressive (red)
	{200, 255, 200, 255}, // 7 passive (light green)
	{255, 40, 40, 255},   // 8 spawned enemy (bright red)
}

const interpK = 20.0

// remoteSnapDist is the position error above which a remote entity is snapped
// rather than smoothed. It should only ever be reached by a genuine teleport
// (admin action, respawn, map change) — if ordinary movement trips it, dead
// reckoning is being fed wrong velocities or wrong world bounds.
const remoteSnapDist = 200.0

// ──────────────────────────────────────────────────────────────
// Cosmetic image bundle
// ──────────────────────────────────────────────────────────────

type cosmeticImgs struct {
	body   *AnimImage // static PNG wrapped as single-frame AnimImage (safe for WASM goroutines)
	head   *AnimImage // may be animated GIF
	hat    *AnimImage // may be animated GIF
	shield *AnimImage // may be animated GIF
	sword  *AnimImage // static PNG
}

// ──────────────────────────────────────────────────────────────
// Character
// ──────────────────────────────────────────────────────────────

// Character represents any moving entity (local player, remote player, or NPC).
type Character struct {
	// Display position (interpolated toward Target for remote entities).
	X, Y float64
	// Authoritative server position.
	TargetX, TargetY float64

	Dir     int
	Moving  bool
	Name    string
	IsNPC       bool
	NPCType     int
	IsLocal     bool
	missedTicks int // ticks since last seen in server state (NPC grace period)
	Gralats  int
	Playtime int // total seconds played (from server)

	// Gani animation state machine
	AnimState string // AnimIdle | AnimWalk | AnimSword | AnimRide

	// Sword hit detection flag
	swordHitDone bool

	// Mount state
	Mounted bool

	// HP / MaxHP for damageable NPCs (0 = immortal)
	HP    int
	MaxHP int

	// Chat bubble above head (replaces name tag while active).
	ChatMsg   string
	chatTimer float64 // counts down from 7s to 0

	// Emoticon thought bubble
	emojiImg   *ebiten.Image
	emojiTimer float64 // counts down from emojiBubbleDuration to 0

	// Cosmetic filenames
	BodyFile   string
	HeadFile   string
	HatFile    string
	ShieldFile string
	SwordFile  string

	// Loaded cosmetic images (swapped under mutex)
	cosmu    sync.Mutex
	cosImgs  cosmeticImgs
	cosDirty bool

	// Animation time counter for animated cosmetics (GIFs).
	cosTime float64

	// Dead reckoning velocity for remote entities (pixels/s).
	// Set from Dir+Moving on each server update; advances TargetX/Y each frame.
	velX, velY float64

	// Hurt / knockback state
	hurtTimer   float64 // counts down; >0 means blinking
	knockVX     float64 // knockback velocity X (pixels/s)
	knockVY     float64 // knockback velocity Y (pixels/s)
	knockTimer  float64 // remaining knockback time

	// Floating damage number above head
	dmgText  string
	dmgTimer float64
	dmgRiseY float64 // accumulated upward offset in pixels

	// Gani players — one per gani file, lazily loaded.
	players map[string]*GaniPlayer
	curAnim string // currently active gani name (e.g. "walk.gani")
}

// NewCharacter allocates a Character.  bodyImg / headImg are ignored; the gani
// system loads its own default images from GANITEMPLATE/res/images/.
func NewCharacter(_ *ebiten.Image, _ *ebiten.Image, x, y float64, name string, isNPC bool, npcType int) *Character {
	return &Character{
		X: x, Y: y,
		TargetX: x, TargetY: y,
		Dir:       2,
		Name:      name,
		IsNPC:     isNPC,
		NPCType:   npcType,
		AnimState: AnimIdle,
		players:   make(map[string]*GaniPlayer),
	}
}

// SetCosmetics updates filenames and triggers async image loading.
func (c *Character) SetCosmetics(bodyFile, headFile, hatFile, shieldFile, swordFile string) {
	if c.BodyFile == bodyFile && c.HeadFile == headFile && c.HatFile == hatFile &&
		c.ShieldFile == shieldFile && c.SwordFile == swordFile {
		return
	}
	c.BodyFile = bodyFile
	c.HeadFile = headFile
	c.HatFile = hatFile
	c.ShieldFile = shieldFile
	c.SwordFile = swordFile
	c.cosDirty = true
}

// StartSword begins (or restarts) the sword swing animation.
// Returns false only when mounted (sword is disabled on horse).
func (c *Character) StartSword() bool {
	if c.AnimState == AnimRide {
		return false
	}
	c.AnimState = AnimSword
	c.swordHitDone = false
	// Always reset so spamming restarts the swing from frame 0.
	if p := c.getPlayer("sword.gani"); p != nil {
		p.Reset()
	}
	return true
}

// SwordHitbox returns the world-space rectangle that can hit enemies during
// the active sword frames.  Returns an empty rectangle otherwise.
func (c *Character) SwordHitbox() image.Rectangle {
	if c.AnimState != AnimSword {
		return image.Rectangle{}
	}
	p := c.getPlayer("sword.gani")
	if p == nil {
		return image.Rectangle{}
	}
	// Frames 1-3 are the active swing in sword.gani (0=windup, 5=recovery).
	if p.Frame < 1 || p.Frame > 3 {
		return image.Rectangle{}
	}
	x, y := int(c.X), int(c.Y)
	const reach = 44
	switch c.Dir {
	case 0: // up
		return image.Rect(x+2, y-reach, x+30, y)
	case 1: // left
		return image.Rect(x-reach, y+2, x, y+30)
	case 2: // down
		return image.Rect(x+2, y+32, x+30, y+32+reach)
	case 3: // right
		return image.Rect(x+32, y+2, x+32+reach, y+30)
	}
	return image.Rectangle{}
}

// SwordJustActivated reports whether the sword entered its active phase this
// tick AND hit detection has not yet been processed.
func (c *Character) SwordJustActivated() bool {
	if c.AnimState != AnimSword || c.swordHitDone {
		return false
	}
	p := c.getPlayer("sword.gani")
	return p != nil && p.Frame >= 1
}

// MarkSwordHitDone prevents duplicate hit detection for the current swing.
func (c *Character) MarkSwordHitDone() { c.swordHitDone = true }

// SetHurt triggers the hurt animation and knockback away from (fromX, fromY).
// damage is the HP amount lost (shown as floating text).
func (c *Character) SetHurt(fromX, fromY float64, damage int) {
	if c.AnimState == AnimDead {
		return // already dead, ignore hits
	}
	dx := c.X - fromX
	dy := c.Y - fromY
	dist := math.Sqrt(dx*dx + dy*dy)
	const knockSpeed = 120.0
	if dist > 0 {
		c.knockVX = dx / dist * knockSpeed
		c.knockVY = dy / dist * knockSpeed
	} else {
		c.knockVX = 0
		c.knockVY = -knockSpeed
	}
	c.knockTimer = 0.12  // knockback duration
	c.hurtTimer = 0.30   // blink duration
	c.AnimState = AnimHurt
	if damage > 0 {
		c.dmgText = fmt.Sprintf("%d", damage)
	} else {
		c.dmgText = "1"
	}
	c.dmgTimer = 1.2
	c.dmgRiseY = 0
	if p := c.getPlayer("hurt.gani"); p != nil {
		p.Reset()
	}
}

// SetChatMsg shows msg in a speech bubble above the character for 7 seconds.
func (c *Character) SetChatMsg(msg string) {
	c.ChatMsg = msg
	c.chatTimer = 7.0
}

// SetEmoji triggers a thought-bubble emoticon above the character.
func (c *Character) SetEmoji(img *ebiten.Image) {
	c.emojiImg = img
	c.emojiTimer = emojiBubbleDuration
}

// drawEmojiBubble renders the thought bubble to the right of the character.
//
// Rise phase (0 → emojiRiseDuration):
//   The image is revealed bottom-to-top: at t=0 nothing shows, at t=1s the
//   full image is visible.  This is done by drawing a growing sub-image that
//   starts at the bottom of the image and expands upward.
//
// Hold phase (emojiRiseDuration → emojiBubbleDuration):
//   The full image stays fixed.
func (c *Character) drawEmojiBubble(screen *ebiten.Image, sx, sy float64) {
	if c.emojiTimer <= 0 || c.emojiImg == nil {
		return
	}

	age := emojiBubbleDuration - c.emojiTimer // 0 at start of life

	iw := c.emojiImg.Bounds().Dx()
	ih := c.emojiImg.Bounds().Dy()

	// Final position: to the right of the character, aligned at head level.
	finalBx := sx + float64(frameW) + 4
	finalBy := sy - float64(ih) - 2

	var srcRect image.Rectangle
	var dstY float64

	if age < emojiRiseDuration {
		// How many rows of the image are visible (grows bottom→top).
		progress := age / emojiRiseDuration
		visH := int(float64(ih)*progress) + 1
		if visH > ih {
			visH = ih
		}
		// Source: the bottom `visH` rows.
		srcRect = image.Rect(0, ih-visH, iw, ih)
		// Destination: aligned so the bottom of the visible slice sits at
		// finalBy+ih (the bottom of the final image position).
		dstY = finalBy + float64(ih-visH)
	} else {
		srcRect = image.Rect(0, 0, iw, ih)
		dstY = finalBy
	}

	op := &ebiten.DrawImageOptions{}
	op.GeoM.Translate(finalBx, dstY)
	screen.DrawImage(c.emojiImg.SubImage(srcRect).(*ebiten.Image), op)
}

// ──────────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────────

// getPlayer returns (and lazily creates) the GaniPlayer for a .gani file.
func (c *Character) getPlayer(name string) *GaniPlayer {
	if p, ok := c.players[name]; ok {
		return p
	}
	p := newGaniPlayer(name)
	c.players[name] = p
	return p
}

// activateAnim switches to the given gani name, resetting if it changed.
func (c *Character) activateAnim(name string) *GaniPlayer {
	if c.curAnim != name {
		c.curAnim = name
		if p := c.getPlayer(name); p != nil {
			p.Reset()
		}
	}
	return c.getPlayer(name)
}

// ──────────────────────────────────────────────────────────────
// Update
// ──────────────────────────────────────────────────────────────

// Update advances the animation state machine and interpolates position.
func (c *Character) Update(dt float64) {
	// Async cosmetic loading
	if c.cosDirty {
		c.cosDirty = false
		body, head, hat, shield, sword := c.BodyFile, c.HeadFile, c.HatFile, c.ShieldFile, c.SwordFile
		go func() {
			// All loading via ReadGameFile+image.Decode — safe from goroutines in WASM.
			// Ebiten image creation is deferred to the main thread inside AnimImage.Frame().
			var imgs cosmeticImgs
			if body != "" {
				imgs.body = loadAnimImage(filepath.Join("assets/offline/levels/bodies", body))
			}
			if head != "" {
				imgs.head = loadAnimImage(filepath.Join("assets/offline/levels/heads", head))
			}
			if hat != "" {
				imgs.hat = loadAnimImage(filepath.Join("assets/offline/levels/hats", hat))
			}
			if shield != "" {
				imgs.shield = loadAnimImage(filepath.Join("assets/offline/levels/shields", shield))
			}
			if sword != "" {
				imgs.sword = loadAnimImage(filepath.Join("assets/offline/levels/swords", sword))
			}
			c.cosmu.Lock()
			c.cosImgs = imgs
			c.cosmu.Unlock()
		}()
	}

	// Advance GIF animation timer.
	c.cosTime += dt

	// Knockback position — applied only for remote/NPC entities.
	// Local player knockback is applied in game.go with collision checking.
	if c.knockTimer > 0 {
		c.knockTimer -= dt
		if !c.IsLocal {
			c.X += c.knockVX * dt
			c.Y += c.knockVY * dt
		}
		if c.knockTimer <= 0 {
			c.knockVX = 0
			c.knockVY = 0
		}
	}

	// Hurt blink + damage text counters
	if c.hurtTimer > 0 {
		c.hurtTimer -= dt
	}
	if c.dmgTimer > 0 {
		c.dmgTimer -= dt
		c.dmgRiseY += 28.0 * dt // float upward
		if c.dmgTimer <= 0 {
			c.dmgText = ""
			c.dmgRiseY = 0
		}
	}

	// Emoji bubble countdown.
	if c.emojiTimer > 0 {
		c.emojiTimer -= dt
		if c.emojiTimer <= 0 {
			c.emojiImg = nil
		}
	}

	// Chat bubble countdown.
	if c.chatTimer > 0 {
		c.chatTimer -= dt
		if c.chatTimer <= 0 {
			c.ChatMsg = ""
			c.chatTimer = 0
		}
	}

	// Position update for remote entities: dead reckoning + interpolation.
	if !c.IsLocal {
		c.applyRemoteMotion(dt)
	}

	// Determine target gani based on animation state.
	// Horse NPCs always use mount.gani (they are never in any other state).
	var targetGani string
	isHorseNPC := c.IsNPC && c.NPCType == NPCTypeHorse
	switch {
	case isHorseNPC:
		targetGani = "mount.gani"
	case c.AnimState == AnimDead:
		targetGani = "dead.gani"
	case c.AnimState == AnimSword:
		targetGani = "sword.gani"
	case c.AnimState == AnimHurt:
		targetGani = "hurt.gani"
	case c.AnimState == AnimSit:
		targetGani = "sit.gani"
	case c.AnimState == AnimJuggle:
		targetGani = "juggle.gani"
	case c.AnimState == AnimHatTrick:
		targetGani = "classic_hattrick_test.gani"
	case c.AnimState == AnimClassicJuggle:
		targetGani = "classic_new_juggle.gani"
	case c.AnimState == AnimPompoms:
		targetGani = "ci_pompoms.gani"
	case c.AnimState == AnimGrab:
		targetGani = "grab.gani"
	case c.AnimState == AnimPush:
		targetGani = "push.gani"
	case c.AnimState == AnimRide:
		targetGani = "mount.gani"
	case c.AnimState == AnimSwim:
		targetGani = "swim.gani"
	case c.AnimState == AnimLava:
		targetGani = "swim.gani" // reuse swim.gani for lava
	case c.Moving:
		targetGani = "walk.gani"
	default:
		targetGani = "idle.gani"
	}

	// Activate gani (resets if switched)
	p := c.activateAnim(targetGani)
	if p == nil {
		return
	}

	switch {
	case targetGani == "mount.gani" && !c.Moving:
		// Freeze horse on still pose when not moving.
		p.Frame = 0
		p.Timer = 0
	case targetGani == "sit.gani":
		// sit.gani is 1 frame — hold it indefinitely.
		p.Frame = 0
		p.Timer = 0
	case targetGani == "grab.gani":
		// Grab is a held pose — freeze on first frame per direction.
		p.Frame = 0
		p.Timer = 0
	case targetGani == "dead.gani" && p.Done:
		// Stay frozen on last death frame.
	default:
		p.Update(dt)
	}

	// Sword: skip the WAIT-14 recovery frame (frame 5) so the player can
	// move again immediately after the last active swing frame.
	if c.AnimState == AnimSword && p.Frame >= 5 {
		p.Done = true
	}

	// Sword completion: return to idle/walk
	if c.AnimState == AnimSword && p.Done {
		c.swordHitDone = false
		if c.Moving {
			c.AnimState = AnimWalk
		} else {
			c.AnimState = AnimIdle
		}
	}

	// Hurt completion: exit when gani done, blink timer expired, or player moves
	if c.AnimState == AnimHurt {
		if c.Moving || p.Done || c.hurtTimer <= 0 {
			if c.Moving {
				c.AnimState = AnimWalk
			} else {
				c.AnimState = AnimIdle
			}
		}
	}

	// Item animations: loop until cancelled by movement (handled in game.go
	// for local player; remote players cancel when server anim state changes).
	if (c.AnimState == AnimClassicJuggle || c.AnimState == AnimPompoms || c.AnimState == AnimJuggle || c.AnimState == AnimHatTrick) && p.Done {
		// Gani finished a non-looping cycle — reset to idle/walk.
		if c.Moving {
			c.AnimState = AnimWalk
		} else {
			c.AnimState = AnimIdle
		}
	}

	// Idle/walk transitions (outside special states)
	if c.AnimState != AnimSword && c.AnimState != AnimRide &&
		c.AnimState != AnimSit && c.AnimState != AnimPush &&
		c.AnimState != AnimDead && c.AnimState != AnimHurt &&
		c.AnimState != AnimGrab &&
		c.AnimState != AnimJuggle && c.AnimState != AnimClassicJuggle &&
		c.AnimState != AnimPompoms && c.AnimState != AnimHatTrick {
		if c.Moving {
			c.AnimState = AnimWalk
		} else {
			c.AnimState = AnimIdle
		}
	}
}

// ──────────────────────────────────────────────────────────────
// Draw
// ──────────────────────────────────────────────────────────────

// Draw renders the character at its world position, offset by the camera.
func (c *Character) Draw(screen *ebiten.Image, camX, camY float64) {
	sx := c.X - camX
	sy := c.Y - camY

	// Frustum cull — use worldBuf dimensions so entities remain visible when zoomed out.
	if sx < -96 || sx > float64(worldBufW)+96 || sy < -96 || sy > float64(worldBufH)+96 {
		return
	}

	// Resolve cosmetic images
	c.cosmu.Lock()
	imgs := c.cosImgs
	c.cosmu.Unlock()

	// Resolve animated frames at the current cosTime (all via AnimImage.Frame — main-thread safe).
	bodyFrame := imgs.body.Frame(c.cosTime)
	headFrame := imgs.head.Frame(c.cosTime)
	hatFrame := imgs.hat.Frame(c.cosTime)
	shieldFrame := imgs.shield.Frame(c.cosTime)
	swordFrame := imgs.sword.Frame(c.cosTime)

	// Build GaniImages for this draw call
	gi := &GaniImages{
		def: GaniDefaultImages(),
	}

	// Horse NPCs: hide all rider sprites, only the HORSE layer shows.
	isHorse := c.IsNPC && c.NPCType == NPCTypeHorse
	if isHorse {
		gi.NoBody = true
		gi.NoHead = true
		gi.NoAttr1 = true
		gi.NoShield = true
	} else {
		if bodyFrame != nil {
			gi.Body = bodyFrame
		}
		if headFrame != nil {
			gi.Head = headFrame
		}
		// No hat selected (empty string) → suppress ATTR1 entirely.
		if c.HatFile == "" {
			gi.NoAttr1 = true
		} else if hatFrame != nil {
			gi.Attr1 = hatFrame
		}
		// Shield: empty string → suppress, otherwise use custom or default.
		if c.ShieldFile == "" {
			gi.NoShield = true
		} else if shieldFrame != nil {
			gi.Shield = shieldFrame
		}
		// Sword: empty string → use default, otherwise use custom.
		if swordFrame != nil {
			gi.Sword = swordFrame
		}
	}

	// Determine which gani player to draw
	ganiName := c.curAnim
	if ganiName == "" {
		ganiName = "idle.gani"
	}
	p := c.getPlayer(ganiName)
	if p == nil || p.Anim == nil {
		c.drawFallback(screen, camX, camY, imgs)
		c.drawEmojiBubble(screen, sx, sy)
		c.drawNameTag(screen, sx, sy)
		return
	}

	// Blink during hurt: skip draw every other ~8-frame window
	if c.hurtTimer > 0 {
		// Toggle visibility ~8 times per second using hurtTimer oscillation
		phase := int(c.hurtTimer*16) % 2
		if phase == 0 {
			// Skip rendering this frame — blink effect
			c.drawEmojiBubble(screen, sx, sy)
			c.drawNameTag(screen, sx, sy)
			return
		}
	}

	// Draw gani frame at character body position
	p.Draw(screen, gi, c.Dir, c.X, c.Y, camX, camY)

	// HP bar for damageable NPCs and damaged players.
	if c.MaxHP > 0 && c.HP > 0 && c.HP < c.MaxHP {
		c.drawHPBar(screen, sx, sy)
	}

	c.drawEmojiBubble(screen, sx, sy)
	c.drawNameTag(screen, sx, sy)
}

// drawFallback draws a plain coloured rectangle when no gani is available.
func (c *Character) drawFallback(screen *ebiten.Image, camX, camY float64, imgs cosmeticImgs) {
	sx := c.X - camX
	sy := c.Y - camY

	// Try custom body first, then fall back to the GANI default body image.
	bodyImg := imgs.body.Frame(c.cosTime)
	if bodyImg == nil {
		if def := GaniDefaultImages(); def != nil {
			bodyImg = def.body
		}
	}
	if bodyImg != nil {
		bodyRect := image.Rect(frameW*c.Dir, 0, frameW*(c.Dir+1), frameH)
		op := &ebiten.DrawImageOptions{}
		op.GeoM.Translate(sx, sy)
		screen.DrawImage(bodyImg.SubImage(bodyRect).(*ebiten.Image), op)
	}
}

// drawHPBar renders a small health bar above the character.
func (c *Character) drawHPBar(screen *ebiten.Image, sx, sy float64) {
	const (
		barW = 30
		barH = 4
	)
	barX := int(sx) + (frameW-barW)/2
	barY := int(sy) - 42

	DrawRect(screen, barX-1, barY-1, barW+2, barH+2, color.RGBA{0, 0, 0, 180})
	DrawRect(screen, barX, barY, barW, barH, color.RGBA{180, 20, 20, 220})
	if fillW := barW * c.HP / c.MaxHP; fillW > 0 {
		DrawRect(screen, barX, barY, fillW, barH, color.RGBA{40, 200, 40, 220})
	}
}

// drawNameTag renders NPC names, chat bubbles, and floating damage text.
// Player names are no longer shown permanently.
func (c *Character) drawNameTag(screen *ebiten.Image, sx, sy float64) {
	nameY := int(sy) - 26

	// Floating damage text (rises upward, fades out).
	if c.dmgText != "" && c.dmgTimer > 0 {
		alpha := uint8(255)
		if c.dmgTimer < 0.5 {
			alpha = uint8(255 * c.dmgTimer / 0.5)
		}
		dmgX := int(sx) + frameW/2 - len(c.dmgText)*fontW/2
		dmgY := nameY - int(c.dmgRiseY)
		DrawText(screen, c.dmgText, dmgX+1, dmgY+1, color.RGBA{0, 0, 0, alpha / 2})
		DrawText(screen, c.dmgText, dmgX, dmgY, color.RGBA{255, 80, 80, alpha})
	}

	// Chat bubble (always shown, for players and NPCs).
	if c.ChatMsg != "" {
		alpha := uint8(220)
		if c.chatTimer < 1.0 {
			alpha = uint8(220 * c.chatTimer)
		}
		chatClr := color.RGBA{255, 255, 255, alpha}
		msg := c.ChatMsg
		if len([]rune(msg)) > 30 {
			msg = string([]rune(msg)[:27]) + "..."
		}
		cx := int(sx) + frameW/2 - len([]rune(msg))*fontW/2
		DrawText(screen, msg, cx, nameY, chatClr)
		_ = basicfont.Face7x13
		return
	}

	// NPC name tag (yellow).
	if c.IsNPC {
		nameX := int(sx) + frameW/2 - len([]rune(c.Name))*fontW/2
		DrawText(screen, c.Name, nameX, nameY, color.RGBA{255, 215, 70, 220})
	}
	// Player names are not shown permanently.
	_ = basicfont.Face7x13
}

// DrawPreview renders the character's idle animation onto dst at screen position
// (dstX, dstY) scaled by scale. offscreen must be a 96×96 image owned by the caller
// (reused each frame to avoid allocations).
func (c *Character) DrawPreview(dst, offscreen *ebiten.Image, dstX, dstY float64, scale float64) {
	offscreen.Clear()

	c.cosmu.Lock()
	imgs := c.cosImgs
	c.cosmu.Unlock()

	gi := &GaniImages{def: GaniDefaultImages()}
	if bodyFrame := imgs.body.Frame(c.cosTime); bodyFrame != nil {
		gi.Body = bodyFrame
	}
	if headFrame := imgs.head.Frame(c.cosTime); headFrame != nil {
		gi.Head = headFrame
	}
	if c.HatFile == "" {
		gi.NoAttr1 = true
	} else if hatPreview := imgs.hat.Frame(c.cosTime); hatPreview != nil {
		gi.Attr1 = hatPreview
	}
	if c.ShieldFile == "" {
		gi.NoShield = true
	} else if shieldFrame := imgs.shield.Frame(c.cosTime); shieldFrame != nil {
		gi.Shield = shieldFrame
	}

	// Place gani origin at (24,24) in the offscreen image so the 48×48 gani box
	// is centered. Body (at offset +8,+16) lands at (32,40) and hats (48×48
	// at origin) fit within the 96×96 canvas.
	p := c.getPlayer("idle.gani")
	if p != nil && p.Anim != nil {
		p.Draw(offscreen, gi, 2,
			float64(ganiOriginDX+24),
			float64(ganiOriginDY+24),
			0, 0)
	} else {
		// Fallback: simple silhouette
		DrawRect(offscreen, 28, 20, 32, 48, color.RGBA{100, 150, 200, 200})
	}

	op := &ebiten.DrawImageOptions{}
	op.GeoM.Scale(scale, scale)
	op.GeoM.Translate(dstX, dstY)
	dst.DrawImage(offscreen, op)
}

// HatThumbRect returns the source rect for a hat thumbnail (down/front-facing).
// Hat sheets are 48×48 per direction, 4 columns: up/left/down/right.
func HatThumbRect() image.Rectangle {
	const hatSz = 48
	return image.Rect(2*hatSz, 0, 3*hatSz, hatSz) // col 2 = down direction
}

// applyRemoteMotion advances a remote entity's predicted position by one frame:
// dead reckoning from the last known velocity, then exponential smoothing of the
// drawn position toward it.
//
// Kept separate from Update so it can be exercised without any of the sprite,
// gani or image machinery.
func (c *Character) applyRemoteMotion(dt float64) {
	// Predict where the entity is NOW rather than where it was when the snapshot
	// was sent, which removes most of the latency positional error.
	if c.Moving {
		c.TargetX += c.velX * dt
		c.TargetY += c.velY * dt

		// Clamp against the world actually loaded (activeWorldW/H), never the
		// compile-time TMX constants: on a larger world those would drag every
		// remote entity back toward the origin every single frame.
		if c.TargetX < 0 {
			c.TargetX = 0
		}
		if c.TargetY < 0 {
			c.TargetY = 0
		}
		if maxX := activeWorldW - float64(frameW); maxX > 0 && c.TargetX > maxX {
			c.TargetX = maxX
		}
		if maxY := activeWorldH - float64(frameH); maxY > 0 && c.TargetY > maxY {
			c.TargetY = maxY
		}
	}

	dx := c.TargetX - c.X
	dy := c.TargetY - c.Y
	if dx*dx+dy*dy > remoteSnapDist*remoteSnapDist {
		// Genuine teleport (admin action, respawn, map change): snap.
		c.X, c.Y = c.TargetX, c.TargetY
		return
	}
	factor := 1 - math.Exp(-interpK*dt)
	c.X += dx * factor
	c.Y += dy * factor
}
