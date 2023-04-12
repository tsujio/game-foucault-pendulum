package main

import (
	"embed"
	"fmt"
	"image"
	"image/color"
	"log"
	"math"
	"math/rand"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/audio"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"
	"github.com/hajimehoshi/ebiten/v2/text"
	"github.com/hajimehoshi/ebiten/v2/vector"
	"github.com/samber/lo"
	logging "github.com/tsujio/game-logging-server/client"
	"github.com/tsujio/game-util/drawutil"
	"github.com/tsujio/game-util/loggingutil"
	"github.com/tsujio/game-util/resourceutil"
	"github.com/tsujio/game-util/touchutil"
)

const (
	gameName            = "foucault-pendulum"
	screenWidth         = 640
	screenHeight        = 480
	circleX             = screenWidth / 2
	circleY             = screenHeight / 2
	circleVerticalScale = 0.7
	pendulumLength      = 955
	pendulumAmplitude   = 220
	pendulumR           = 10.0
	gravity             = 9.8 / 60
)

//go:embed resources/*.ttf resources/*.dat resources/bgm-*.wav resources/secret
var resources embed.FS

var (
	fontL, fontM, fontS = resourceutil.ForceLoadFont(resources, "resources/PressStart2P-Regular.ttf", nil)
	audioContext        = audio.NewContext(48000)
	gameStartAudioData  = resourceutil.ForceLoadDecodedAudio(resources, "resources/魔王魂 効果音 システム49.mp3.dat", audioContext)
	gameOverAudioData   = resourceutil.ForceLoadDecodedAudio(resources, "resources/魔王魂 効果音 システム32.mp3.dat", audioContext)
	scoreUpAudioData    = resourceutil.ForceLoadDecodedAudio(resources, "resources/魔王魂 効果音 物音15.mp3.dat", audioContext)
	bgmPlayer           = resourceutil.ForceCreateBGMPlayer(resources, "resources/bgm-foucault-pendulum.wav", audioContext)
)

var (
	emptyImage = func() *ebiten.Image {
		img := ebiten.NewImage(3, 3)
		img.Fill(color.White)
		return img
	}()
	emptySubImage = emptyImage.SubImage(image.Rect(1, 1, 2, 2)).(*ebiten.Image)
)

type PolarCoordinates struct {
	r, theta float64
}

func (p *PolarCoordinates) toScreen() (float64, float64) {
	x, y := p.r*math.Cos(p.theta), p.r*math.Sin(p.theta)
	return x + circleX, y*circleVerticalScale + circleY
}

func (p *PolarCoordinates) fromScreen(x, y float64) {
	x -= circleX
	y -= circleY
	y /= circleVerticalScale
	p.r = math.Sqrt(x*x + y*y)
	p.theta = math.Atan2(y, x)
}

type Mover struct {
	delay int
	delta float64
}

func (m *Mover) move(ticks uint, pos *PolarCoordinates) {
	if int(ticks)-m.delay > 0 {
		x, y := pos.toScreen()
		pos.fromScreen(x+m.delta, y)
	}
}

type Coin struct {
	ticks uint
	pos   *PolarCoordinates
	mover *Mover
	r     float64
	hit   bool
}

var coinImage = drawutil.CreatePatternImage([][]rune{
	[]rune("  ##  "),
	[]rune(" #.## "),
	[]rune("#.####"),
	[]rune("####/#"),
	[]rune("####/#"),
	[]rune("####/#"),
	[]rune("####/#"),
	[]rune(" ##/# "),
	[]rune("  ##  "),
}, &drawutil.CreatePatternImageOption[rune]{
	ColorMap: map[rune]color.Color{
		'#': color.RGBA{0xff, 0xe0, 0, 0xff},
		'/': color.RGBA{0xf5, 0xc0, 0, 0xff},
		'.': color.RGBA{0xff, 0xf0, 0, 0xff},
	},
})

func (c *Coin) draw(screen *ebiten.Image) {
	_, h := coinImage.Size()
	opts := &ebiten.DrawImageOptions{}
	opts.GeoM.Scale(c.r*2/float64(h), c.r*2/float64(h))
	x, y := c.pos.toScreen()
	drawutil.DrawImageAt(screen, coinImage, x, y, opts)
}

type CoinHitEffect struct {
	ticks uint
	pos   *PolarCoordinates
	gain  int
}

func (e *CoinHitEffect) draw(screen *ebiten.Image) {
	x, y := e.pos.toScreen()
	y -= 10.0 * math.Sin(float64(e.ticks)*math.Pi/60)
	text.Draw(screen, fmt.Sprintf("%+d", e.gain), fontM.Face, int(x), int(y), color.RGBA{0xff, 0xff, 0, 0xff})
}

type Enemy struct {
	ticks uint
	pos   *PolarCoordinates
	mover *Mover
	r     float64
}

var enemyImages = drawutil.CreatePatternImageArray([][][]rune{
	{
		[]rune(" ####  "),
		[]rune("#.#.###"),
		[]rune("###### "),
		[]rune("#######"),
		[]rune("# ##  #"),
	},
	{
		[]rune(" ####  "),
		[]rune("#######"),
		[]rune("#.#.## "),
		[]rune("#######"),
		[]rune("#   # #"),
	},
}, &drawutil.CreatePatternImageOption[rune]{
	ColorMap: map[rune]color.Color{
		'#': color.Black,
		'.': color.White,
	},
})

func (e *Enemy) draw(screen *ebiten.Image) {
	x, y := e.pos.toScreen()

	w, _ := enemyImages[0].Size()
	scale := e.r * 2 / float64(w)

	opts := &ebiten.DrawImageOptions{}
	opts.GeoM.Scale(scale, scale)

	if e.mover.delta > 0 {
		opts.GeoM.Scale(-1, 1)
	}

	img := enemyImages[e.ticks/30%2]

	drawutil.DrawImageAt(screen, img, x, y, opts)
}

type GameMode int

const (
	GameModeTitle GameMode = iota
	GameModePlaying
	GameModeGameOver
)

type Game struct {
	playerID           string
	playID             string
	fixedRandomSeed    int64
	touchContext       *touchutil.TouchContext
	random             *rand.Rand
	mode               GameMode
	ticksFromModeStart uint64
	score              int
	hold               bool
	pendulumX          float64
	pendulumVx         float64
	pendulumRotation   float64
	skyImg             *ebiten.Image
	coins              []*Coin
	coinHitEffects     []*CoinHitEffect
	enemies            []*Enemy
	lastPendulumTicks  uint64
}

func (g *Game) Update() error {
	g.touchContext.Update()

	g.ticksFromModeStart++

	loggingutil.SendTouchLog(gameName, g.playerID, g.playID, g.ticksFromModeStart, g.touchContext)

	switch g.mode {
	case GameModeTitle:
		if g.touchContext.IsJustTouched() {
			g.pendulumX = pendulumAmplitude

			g.setNextMode(GameModePlaying)

			loggingutil.SendLog(gameName, g.playerID, g.playID, map[string]interface{}{
				"action": "start_game",
			})

			audio.NewPlayerFromBytes(audioContext, gameStartAudioData).Play()

			bgmPlayer.Rewind()
			bgmPlayer.Play()
		}
	case GameModePlaying:
		if g.ticksFromModeStart%600 == 0 {
			loggingutil.SendLog(gameName, g.playerID, g.playID, map[string]interface{}{
				"action": "playing",
				"ticks":  g.ticksFromModeStart,
				"score":  g.score,
			})
		}

		if g.touchContext.IsJustTouched() {
			g.hold = true
		}
		if g.touchContext.IsJustReleased() {
			g.hold = false
		}

		if g.hold {
			g.pendulumRotation += math.Pi / 800
			if g.pendulumRotation > math.Pi*2 {
				g.pendulumRotation -= math.Pi * 2
			}
		}

		g.pendulumVx += -gravity / pendulumLength * g.pendulumX
		g.pendulumX += g.pendulumVx

		if (g.ticksFromModeStart-g.lastPendulumTicks)%480 == 0 {
			g.pendulumVx = 0
			g.pendulumX = pendulumAmplitude
			g.lastPendulumTicks = g.ticksFromModeStart
		}

		enemyAppearanceRate := lo.
			If(g.ticksFromModeStart < 1800, 180).
			ElseIf(g.ticksFromModeStart < 2400, 120).
			ElseIf(g.ticksFromModeStart < 3000, 100).
			ElseIf(g.ticksFromModeStart < 3600, 60).
			Else(40)

		if g.random.Int()%enemyAppearanceRate == 0 {
			x := lo.If(g.random.Int()%2 == 0, -50.0).Else(screenWidth + 50.0)
			moverDelta := lo.If(x < 0, 1.0).Else(-1.0)

			var y float64
			for {
				y = (screenHeight-150)*g.random.Float64() + 110
				if math.Abs(y-circleY) > 30 {
					break
				}
			}

			pos := &PolarCoordinates{}
			pos.fromScreen(x, y)

			for i := 0; i < 6; i++ {
				p := *pos
				g.coins = append(g.coins, &Coin{
					pos: &p,
					mover: &Mover{
						delay: (i + 1) * 20,
						delta: moverDelta,
					},
					r: lo.If(i < 3, 10.0).Else(7.0),
				})
			}

			e := &Enemy{
				pos: pos,
				mover: &Mover{
					delay: 0,
					delta: moverDelta,
				},
				r: 10,
			}
			g.enemies = append(g.enemies, e)
		}

		for _, c := range g.coins {
			if math.Pow(g.pendulumX*math.Cos(g.pendulumRotation)-c.pos.r*math.Cos(c.pos.theta), 2)+
				math.Pow(g.pendulumX*math.Sin(g.pendulumRotation)-c.pos.r*math.Sin(c.pos.theta), 2) <
				math.Pow(pendulumR+c.r, 2) {
				c.hit = true

				gain := lo.If(c.r <= 5.0, 100).ElseIf(c.r <= 7.0, 300).Else(1000)

				g.coinHitEffects = append(g.coinHitEffects, &CoinHitEffect{
					pos: &PolarCoordinates{
						r:     c.pos.r,
						theta: c.pos.theta,
					},
					gain: gain,
				})

				g.score += gain

				audio.NewPlayerFromBytes(audioContext, scoreUpAudioData).Play()
			}

			c.ticks++

			c.mover.move(c.ticks, c.pos)
		}

		for _, e := range g.coinHitEffects {
			e.ticks++
		}

		for _, e := range g.enemies {
			if math.Pow(g.pendulumX*math.Cos(g.pendulumRotation)-e.pos.r*math.Cos(e.pos.theta), 2)+
				math.Pow(g.pendulumX*math.Sin(g.pendulumRotation)-e.pos.r*math.Sin(e.pos.theta), 2) <
				math.Pow(pendulumR+e.r, 2) {
				loggingutil.SendLog(gameName, g.playerID, g.playID, map[string]interface{}{
					"action": "game_over",
					"score":  g.score,
				})

				g.setNextMode(GameModeGameOver)

				loggingutil.RegisterScoreToRankingAsync(gameName, g.playerID, g.playID, g.score)

				audio.NewPlayerFromBytes(audioContext, gameOverAudioData).Play()

				break
			}

			e.ticks++

			e.mover.move(e.ticks, e.pos)
		}

		g.coins = lo.Filter(g.coins, func(c *Coin, _ int) bool {
			x, y := c.pos.toScreen()
			return x > -100 && x < screenWidth+100 && y > -100 && y < screenHeight+100 && !c.hit
		})

		g.coinHitEffects = lo.Filter(g.coinHitEffects, func(e *CoinHitEffect, _ int) bool {
			return e.ticks < 60
		})

		g.enemies = lo.Filter(g.enemies, func(e *Enemy, _ int) bool {
			x, y := e.pos.toScreen()
			return x > -100 && x < screenWidth+100 && y > -100 && y < screenHeight+100
		})

	case GameModeGameOver:
		if g.ticksFromModeStart > 60 && g.touchContext.IsJustTouched() {
			g.initialize()
			bgmPlayer.Pause()
		}
	}

	return nil
}

func (g *Game) drawTitleText(screen *ebiten.Image) {
	titleTexts := []string{"FOUCAULT PENDULUM"}
	for i, s := range titleTexts {
		text.Draw(screen, s, fontL.Face, screenWidth/2-len(s)*int(fontL.FaceOptions.Size)/2, 110+i*int(fontL.FaceOptions.Size*1.8), color.White)
	}

	usageTexts := []string{"[HOLD] Rotate the Earth"}
	for i, s := range usageTexts {
		text.Draw(screen, s, fontS.Face, screenWidth/2-len(s)*int(fontS.FaceOptions.Size)/2, 310+i*int(fontS.FaceOptions.Size*1.8), color.White)
	}

	creditTexts := []string{"CREATOR: NAOKI TSUJIO", "FONT: Press Start 2P by CodeMan38", "SOUND EFFECT: MaouDamashii", "POWERED BY Ebitengine"}
	for i, s := range creditTexts {
		text.Draw(screen, s, fontS.Face, screenWidth/2-len(s)*int(fontS.FaceOptions.Size)/2, 400+i*int(fontS.FaceOptions.Size*1.8), color.White)
	}
}

func (g *Game) drawScore(screen *ebiten.Image) {
	t := fmt.Sprintf("%d", g.score)
	text.Draw(screen, t, fontS.Face, screenWidth-len(t)*int(fontS.FaceOptions.Size)-10, 25, color.White)
}

func (g *Game) drawGameOverText(screen *ebiten.Image) {
	gameOverTexts := []string{"GAME OVER"}
	for i, s := range gameOverTexts {
		text.Draw(screen, s, fontL.Face, screenWidth/2-len(s)*int(fontL.FaceOptions.Size)/2, 170+i*int(fontL.FaceOptions.Size*1.8), color.White)
	}

	scoreText := []string{"YOUR SCORE IS", fmt.Sprintf("%d!", g.score)}
	for i, s := range scoreText {
		text.Draw(screen, s, fontM.Face, screenWidth/2-len(s)*int(fontM.FaceOptions.Size)/2, 230+i*int(fontM.FaceOptions.Size*1.8), color.White)
	}
}

func (g *Game) drawSky(screen *ebiten.Image) {
	op := &ebiten.DrawImageOptions{}
	op.GeoM.Rotate(g.pendulumRotation)
	drawutil.DrawImageAt(screen, g.skyImg, screenWidth/2, -100, op)
}

func (g *Game) drawSurface(screen *ebiten.Image) {
	var path vector.Path
	path.MoveTo(0, 100)
	ctlX, ctlY := 180, 50
	path.CubicTo(float32(ctlX), float32(ctlY), screenWidth-float32(ctlX), float32(ctlY), screenWidth, 100)
	path.LineTo(screenWidth, screenHeight)
	path.LineTo(0, screenHeight)
	vs, is := path.AppendVerticesAndIndicesForFilling(nil, nil)
	for i := range vs {
		vs[i].SrcX = 1
		vs[i].SrcY = 1
		vs[i].ColorR = 0x33 / float32(0xff)
		vs[i].ColorG = 0xcc / float32(0xff)
		vs[i].ColorB = 0x66 / float32(0xff)
	}
	op := &ebiten.DrawTrianglesOptions{
		AntiAlias: true,
	}
	screen.DrawTriangles(vs, is, emptySubImage, op)
}

func (g *Game) drawPendulum(screen *ebiten.Image) {
	x := g.pendulumX*math.Cos(g.pendulumRotation) + circleX
	y := g.pendulumX*math.Sin(g.pendulumRotation)*circleVerticalScale + circleY

	clr := color.RGBA{0xe5, 0xe5, 0xe5, 0xff}
	ebitenutil.DrawLine(screen, x, y, circleX, circleY-pendulumLength, clr)
	ebitenutil.DrawCircle(screen, x, y, pendulumR, clr)
}

var circleImage = func() *ebiten.Image {
	img := ebiten.NewImage(screenWidth, screenHeight)
	img.Fill(color.Transparent)
	for i := 0; i < 5; i++ {
		scale := float32(i+1) / 5.0
		vector.StrokeCircle(img, circleX, circleY, pendulumAmplitude*scale, 1, color.RGBA{0x3a, 0x3a, 0x3a, 0xff}, true)
	}
	return img
}()

func (g *Game) drawCircle(screen *ebiten.Image) {
	opts := &ebiten.DrawImageOptions{}
	opts.GeoM.Scale(1.0, circleVerticalScale)
	drawutil.DrawImageAt(screen, circleImage, circleX, circleY, opts)
}

func (g *Game) drawGuide(screen *ebiten.Image) {
	pos := &PolarCoordinates{
		r:     pendulumAmplitude,
		theta: g.pendulumRotation,
	}
	x0, y0 := pos.toScreen()
	pos.theta += math.Pi
	x1, y1 := pos.toScreen()
	vector.StrokeLine(screen, float32(x0), float32(y0), float32(x1), float32(y1), 2, color.RGBA{0x3a, 0x3a, 0x3a, 0xff}, true)
}

func (g *Game) Draw(screen *ebiten.Image) {
	screen.Fill(color.Black)

	switch g.mode {
	case GameModeTitle:
		g.drawSky(screen)

		g.drawSurface(screen)

		g.drawCircle(screen)

		g.drawPendulum(screen)

		g.drawTitleText(screen)
	case GameModePlaying:
		g.drawSky(screen)

		g.drawSurface(screen)

		g.drawCircle(screen)

		g.drawGuide(screen)

		for _, c := range g.coins {
			c.draw(screen)
		}

		for _, e := range g.enemies {
			e.draw(screen)
		}

		g.drawPendulum(screen)

		for _, e := range g.coinHitEffects {
			e.draw(screen)
		}

		g.drawScore(screen)
	case GameModeGameOver:
		g.drawSky(screen)

		g.drawSurface(screen)

		g.drawCircle(screen)

		g.drawGuide(screen)

		for _, c := range g.coins {
			c.draw(screen)
		}

		for _, e := range g.enemies {
			e.draw(screen)
		}

		g.drawPendulum(screen)

		for _, e := range g.coinHitEffects {
			e.draw(screen)
		}

		g.drawScore(screen)

		g.drawGameOverText(screen)
	}
}

func (g *Game) Layout(outsideWidth, outsideHeight int) (int, int) {
	return screenWidth, screenHeight
}

func (g *Game) setNextMode(mode GameMode) {
	g.mode = mode
	g.ticksFromModeStart = 0
}

func (g *Game) initialize() {
	var playID string
	if playIDObj, err := uuid.NewRandom(); err == nil {
		playID = playIDObj.String()
	}
	g.playID = playID

	var seed int64
	if g.fixedRandomSeed != 0 {
		seed = g.fixedRandomSeed
	} else {
		seed = time.Now().Unix()
	}

	loggingutil.SendLog(gameName, g.playerID, g.playID, map[string]interface{}{
		"action": "initialize",
		"seed":   seed,
	})

	g.random = rand.New(rand.NewSource(seed))
	g.score = 0
	g.hold = false
	g.pendulumX = 0
	g.pendulumVx = 0
	g.pendulumRotation = 0
	g.skyImg = nil
	g.coins = nil
	g.coinHitEffects = nil
	g.enemies = nil
	g.lastPendulumTicks = 0

	skyImgLength := math.Max(screenWidth, screenHeight) * 1.2
	skyImg := ebiten.NewImage(int(skyImgLength), int(skyImgLength))
	skyImg.Fill(color.Black)
	for i := 0; i < 500; i++ {
		ebitenutil.DrawCircle(skyImg, skyImgLength*g.random.Float64(), skyImgLength*g.random.Float64(), math.Max(1.0+0.5*g.random.NormFloat64(), 0.5), color.White)
	}
	g.skyImg = skyImg

	g.setNextMode(GameModeTitle)
}

func main() {
	if os.Getenv("GAME_LOGGING") == "1" {
		secret, err := resources.ReadFile("resources/secret")
		if err == nil {
			logging.Enable(string(secret))
		}
	} else {
		logging.Disable()
	}

	var randomSeed int64
	if seed, err := strconv.Atoi(os.Getenv("GAME_RAND_SEED")); err == nil {
		randomSeed = int64(seed)
	}

	playerID := os.Getenv("GAME_PLAYER_ID")
	if playerID == "" {
		if playerIDObj, err := uuid.NewRandom(); err == nil {
			playerID = playerIDObj.String()
		}
	}

	ebiten.SetWindowSize(screenWidth, screenHeight)
	ebiten.SetWindowTitle("Foucault Pendulum")

	game := &Game{
		playerID:        playerID,
		fixedRandomSeed: randomSeed,
		touchContext:    touchutil.CreateTouchContext(),
	}
	game.initialize()

	if err := ebiten.RunGame(game); err != nil {
		log.Fatal(err)
	}
}
