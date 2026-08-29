// Command spritegen draws the game's pixel art and writes it to the client.
//
// The art is generated rather than drawn by hand because there is no artist,
// and generated deliberately rather than left as flat rectangles because flat
// rectangles stopped being useful the moment movement was worth judging. A
// mannequin that leans into a run and a slime that squashes tell you the
// simulation is doing what you think; a blue box does not.
//
// The generator is the source and the PNGs are the output, in the same way the
// golden fixtures work: run `make sprites`, look at the diff, commit both. The
// sheets live under the client rather than in content/ because the server has
// no use for them -- art is presentation, and putting it in content would
// change the content hash every time a colour did, kicking every player out.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("out", "client/public/sprites", "directory to write sprite sheets into")
	flag.Parse()

	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "spritegen:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	sheets := map[string]*canvas{
		"terrain.png": drawTerrainSheet(),
		"player.png":  drawPlayerSheet(),
		"mobs.png":    drawMobSheet(),
		"drops.png":   drawDropSheet(),
		// Two depths. The far layer is barely lighter than the sky and the
		// near one is closer to the terrain, so they separate by value rather
		// than by detail -- detail at that distance reads as clutter.
		"hills-far.png":  drawBackdrop(512, 150, ramp{rgb(0x141a26), rgb(0x1a2130), rgb(0x232c3f)}, 0x1234, 3),
		"hills-near.png": drawBackdrop(512, 190, ramp{rgb(0x1a2230), rgb(0x232d3f), rgb(0x2f3b52)}, 0x9876, 4),
	}

	for name, c := range sheets {
		if err := writePNG(filepath.Join(dir, name), c); err != nil {
			return err
		}
		fmt.Printf("%-16s %4d x %d\n", name, c.w, c.h)
	}

	// The manifest, so the client never hard-codes a frame position. Two
	// numbers in two places always drift, and the drift shows up as a sprite
	// sliced down the middle rather than as anything that fails.
	return writeManifest(filepath.Join(dir, "sprites.json"))
}

// manifest describes every sheet's geometry for the renderer.
type manifest struct {
	Player struct {
		FrameW  int            `json:"frameW"`
		FrameH  int            `json:"frameH"`
		BodyW   int            `json:"bodyW"`
		BodyH   int            `json:"bodyH"`
		Pad     int            `json:"pad"`
		Anim    map[string]int `json:"anim"`
		Lengths map[string]int `json:"lengths"`
	} `json:"player"`

	Terrain struct {
		Tile  int            `json:"tile"`
		Index map[string]int `json:"index"`
	} `json:"terrain"`

	// Mobs are keyed by "WxH" because that is all a client knows about a mob:
	// the snapshot carries a display name and a size, not a content id. Size
	// is the stable half of that, and it is generated from content here, so
	// the two cannot disagree. An appearance id on the entity would be better
	// and is a protocol change this does not need yet.
	Mobs map[string]mobEntry `json:"mobs"`

	Drops struct {
		Size       int `json:"size"`
		CoinFrames int `json:"coinFrames"`
		ItemFrame  int `json:"itemFrame"`
	} `json:"drops"`
}

type mobEntry struct {
	X      int `json:"x"`
	W      int `json:"w"`
	H      int `json:"h"`
	Frames int `json:"frames"`
}

func writeManifest(path string) error {
	var m manifest

	m.Player.FrameW, m.Player.FrameH = manW, manH
	m.Player.BodyW, m.Player.BodyH = bodyW, bodyH
	m.Player.Pad = framePad
	m.Player.Anim = map[string]int{
		"idle":   0,
		"run":    len(idleCycle),
		"jump":   len(idleCycle) + len(runCycle),
		"fall":   len(idleCycle) + len(runCycle) + 1,
		"climb":  len(idleCycle) + len(runCycle) + 2,
		"attack": len(idleCycle) + len(runCycle) + 2 + len(climbPoses),
	}
	m.Player.Lengths = map[string]int{
		"idle":   len(idleCycle),
		"run":    len(runCycle),
		"jump":   1,
		"fall":   1,
		"climb":  len(climbPoses),
		"attack": len(attackPoses),
	}

	m.Terrain.Tile = tile
	m.Terrain.Index = map[string]int{
		"groundTop":   int(tileGroundTop),
		"groundFill":  int(tileGroundFill),
		"groundLeft":  int(tileGroundLeft),
		"groundRight": int(tileGroundRight),
		"platform":    int(tilePlatform),
		"rope":        int(tileRope),
	}

	m.Mobs = map[string]mobEntry{}
	x := 0
	for _, mob := range mobLayout {
		m.Mobs[sizeKey(mob.w, mob.h)] = mobEntry{
			X: x, W: mob.w, H: mob.h, Frames: mob.frames,
		}
		x += mob.w * mob.frames
	}

	m.Drops.Size = dropSize
	m.Drops.CoinFrames = 4
	m.Drops.ItemFrame = 4

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// playerFrames is the frame order in player.png. The client indexes by these,
// so the order is a contract rather than a layout detail.
var playerFrames = func() []pose {
	var out []pose
	out = append(out, idleCycle...)   // 0,1   idle
	out = append(out, runCycle...)    // 2-5   run
	out = append(out, jumpPose)       // 6     jump
	out = append(out, fallPose)       // 7     fall
	out = append(out, climbPoses...)  // 8,9   climb
	out = append(out, attackPoses...) // 10,11 attack
	return out
}()

func drawPlayerSheet() *canvas {
	sheet := newCanvas(manW*len(playerFrames), manH)

	for i, p := range playerFrames {
		// The weapon appears only in the attack frames. A character
		// permanently holding a sword cannot be seen to draw one.
		weapon := i >= len(playerFrames)-len(attackPoses)
		sheet.blit(drawMannequin(p, weapon), i*manW, 0)
	}
	return sheet
}

// mobFrame is one mob's slot on the shared sheet.
type mobFrame struct {
	id     string
	w, h   int
	frames int
}

// mobLayout is the mob sheet's contract with the client: which mob is where,
// and how wide its frames are. Sizes come from content/mobs, because a sprite
// that does not match its hitbox makes every "that should have hit" argument
// unanswerable.
var mobLayout = []mobFrame{
	{"slime_green", 32, 28, 2},
	{"boar_wild", 44, 36, 2},
	{"slime_king", 64, 56, 2},
}

func drawMobSheet() *canvas {
	width, height := 0, 0
	for _, m := range mobLayout {
		width += m.w * m.frames
		if m.h > height {
			height = m.h
		}
	}

	sheet := newCanvas(width, height)
	x := 0
	for _, m := range mobLayout {
		for f := 0; f < m.frames; f++ {
			var c *canvas
			switch m.id {
			case "boar_wild":
				c = drawBoar(m.w, m.h, f*2-1, boar)
			case "slime_king":
				c = drawSlimeKing(m.w, m.h, f*3, royal)
			default:
				c = drawSlime(m.w, m.h, f*3, slime)
			}
			// Bottom-aligned: mobs stand on the ground, and a sheet aligned
			// at the top would sink the short ones into it.
			sheet.blit(c, x, height-m.h)
			x += m.w
		}
	}
	return sheet
}

// dropSize is the frame size for ground loot, matching the 20-unit drop body.
const dropSize = 20

func drawDropSheet() *canvas {
	const coinFrames = 4
	sheet := newCanvas(dropSize*(coinFrames+1), dropSize)

	for f := 0; f < coinFrames; f++ {
		// A ping-pong of the narrowing, so the coin turns rather than
		// shrinking away and snapping back.
		narrow := f
		if f > coinFrames/2 {
			narrow = coinFrames - f
		}
		sheet.blit(drawCoin(dropSize, narrow*3), f*dropSize, 0)
	}
	sheet.blit(drawItemDrop(dropSize), coinFrames*dropSize, 0)
	return sheet
}

// sizeKey is how the client finds a mob's sprite: by the only thing a snapshot
// tells it about a mob's appearance.
func sizeKey(w, h int) string { return fmt.Sprintf("%dx%d", w, h) }

func writePNG(path string, c *canvas) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// No compression tuning: these are a few kilobytes each, and the default
	// keeps the output byte-identical across Go versions.
	if err := png.Encode(f, c.img); err != nil {
		return err
	}
	return f.Close()
}
