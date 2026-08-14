package main

// PlayerState is the synchronized state of a connected player.
type PlayerState struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Dir       int     `json:"dir"`
	Moving    bool    `json:"moving"`
	Body      string  `json:"body,omitempty"`
	Head      string  `json:"head,omitempty"`
	Hat       string  `json:"hat,omitempty"`
	Shield    string  `json:"shield,omitempty"`
	Sword     string  `json:"sword,omitempty"`
	Gralats   int     `json:"gralats,omitempty"`
	Playtime  int     `json:"playtime,omitempty"` // total seconds played (including current session)
	AnimState string  `json:"anim,omitempty"`
	Mounted   bool    `json:"mounted,omitempty"`
	HP        int     `json:"hp,omitempty"`
	MaxHP     int     `json:"maxHp,omitempty"`
}

// NPCState is the synchronized state of an NPC.
type NPCState struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Dir       int     `json:"dir"`
	Moving    bool    `json:"moving"`
	NPCType   int     `json:"npcType"`
	HP        int     `json:"hp"`
	MaxHP     int     `json:"maxHp"`
	MountedBy string  `json:"mountedBy,omitempty"`
	AnimState string  `json:"anim,omitempty"`
}

// GralatPickup is a collectable coin in the world.
type GralatPickup struct {
	ID    string  `json:"id"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Value int     `json:"value"` // 1 | 5 | 30 | 100
	// MapID is the map instance this pickup lives on. Server-side only —
	// clients only ever receive pickups for the map they are on.
	MapID string `json:"-"`
}

// InventoryItem represents a usable item in a player's inventory.
type InventoryItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Gani     string `json:"gani"`
	Quantity int    `json:"quantity"`
}

// WorldSpawnItem is an admin-spawned decorative/shop item visible to all players
// on the same map.
type WorldSpawnItem struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	SpritePath string  `json:"sprite"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Price      int     `json:"price"`
	ItemID     string  `json:"item_id,omitempty"`
	// MapID is the map instance this item lives on (persisted as
	// world_items.map_name). Server-side only — clients only ever receive
	// items for the map they are on.
	MapID string `json:"-"`
}
