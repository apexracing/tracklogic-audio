package audio

import (
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"sync/atomic"

	"github.com/gen2brain/malgo"
)

// DeviceInfo represents an audio playback or capture device.
type DeviceInfo struct {
	ID        string
	Name      string
	IsDefault bool
}

// preloadedSound holds decoded WAV PCM data ready for playback.
type preloadedSound struct {
	pcmData       []byte
	numChannels   uint16
	bitsPerSample uint16
	sampleRate    uint32
}

// AudioPlayerEngine manages the shared audio context, cached playback sounds,
// Players, and Recorders. The name is retained for backward compatibility;
// new code that uses capture may prefer the AudioEngine alias.
//
// Typical usage:
//
//	engine.Init()
//	defer engine.Destroy()
//	engine.Preload("beep", "beep.wav")
//	engine.PlaySound("beep", "") // play on default device
type AudioPlayerEngine struct {
	ctx       *malgo.AllocatedContext
	sounds    map[string]*preloadedSound // name → decoded PCM
	cache     map[string]*Player         // "name|deviceID" → Player
	players   []*Player                  // all Players for cleanup
	recorders map[*Recorder]struct{}     // all Recorders for cleanup

	masterVolume atomic.Uint32 // float32 bits, 1.0 = full volume
	masterGain   atomic.Uint32 // float32 bits, 1.0 = unity gain
	mu           sync.Mutex
}

// AudioEngine is the preferred name when an engine is used for both
// playback and capture. AudioPlayerEngine remains fully compatible.
type AudioEngine = AudioPlayerEngine

// Init initializes the audio backend. Must be called first.
func (e *AudioPlayerEngine) Init() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx != nil {
		return nil
	}
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(message string) {
		// discard malgo log output
	})
	if err != nil {
		return err
	}
	e.ctx = ctx
	e.sounds = make(map[string]*preloadedSound)
	e.cache = make(map[string]*Player)
	e.recorders = make(map[*Recorder]struct{})
	e.masterVolume.Store(math.Float32bits(1.0))
	e.masterGain.Store(math.Float32bits(1.0))
	return nil
}

// Destroy stops and closes all Players and Recorders, then releases the audio backend.
func (e *AudioPlayerEngine) Destroy() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx == nil {
		return nil
	}
	for recorder := range e.recorders {
		_ = recorder.close()
	}
	for _, p := range e.players {
		p.deviceMu.Lock()
		p.device.Stop()
		p.device.Uninit()
		p.deviceMu.Unlock()
	}
	e.players = nil
	e.cache = nil
	e.sounds = nil
	e.recorders = nil
	e.ctx.Uninit()
	e.ctx.Free()
	e.ctx = nil
	return nil
}

// ListDevices returns all available audio playback devices.
func (e *AudioPlayerEngine) ListDevices() ([]DeviceInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx == nil {
		return nil, ErrNotInitialized
	}
	return listDevicesWithCtx(e.ctx, malgo.Playback)
}

// ListCaptureDevices returns all available audio input devices.
func (e *AudioPlayerEngine) ListCaptureDevices() ([]DeviceInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx == nil {
		return nil, ErrNotInitialized
	}
	return listDevicesWithCtx(e.ctx, malgo.Capture)
}

// Preload decodes a WAV file and caches the PCM data in memory under the given name.
// Subsequent PlaySound calls use the cached data with no disk I/O.
func (e *AudioPlayerEngine) Preload(name string, wavPath string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx == nil {
		return ErrNotInitialized
	}
	if _, ok := e.sounds[name]; ok {
		return nil // already loaded
	}
	snd, err := loadSound(wavPath)
	if err != nil {
		return err
	}
	e.sounds[name] = snd
	return nil
}

// PreloadWAV decodes a WAV stream and caches it under name. The reader is not
// retained and may be an embedded resource; it must be positioned at the WAV start.
func (e *AudioPlayerEngine) PreloadWAV(name string, r io.ReadSeeker) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx == nil {
		return ErrNotInitialized
	}
	if _, ok := e.sounds[name]; ok {
		return nil
	}
	snd, err := loadSoundReader(r)
	if err != nil {
		return err
	}
	e.sounds[name] = snd
	return nil
}

// PlaySound plays a preloaded sound on the given device.
// If a Player for this (sound, device) combination exists, it reuses it (low latency).
// Otherwise a new Player is created and cached for future calls.
// Returns the Player so callers can wait on Done().
// Pass deviceID="" for the default device.
func (e *AudioPlayerEngine) PlaySound(name string, deviceID string) (*Player, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx == nil {
		return nil, ErrNotInitialized
	}
	return e.playSoundLocked(name, deviceID, false)
}

// PlayLoop plays a preloaded sound and sets the player into loop mode.
// This is the same lifecycle as PlaySound, except the Player remains active after
// each playback pass and never auto-stops for that track.
func (e *AudioPlayerEngine) PlayLoop(name string, deviceID string) (*Player, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx == nil {
		return nil, ErrNotInitialized
	}
	return e.playSoundLocked(name, deviceID, true)
}

// playSoundLocked resolves and replays/creates a Player. Must be called with e.mu held.
func (e *AudioPlayerEngine) playSoundLocked(name string, deviceID string, looping bool) (*Player, error) {
	return e.playSoundWithVolumeLocked(name, deviceID, looping, nil)
}

// PlayLoopWithVolume prepares the voice volume before playback begins, without
// changing other voices or the shared engine master volume.
func (e *AudioPlayerEngine) PlayLoopWithVolume(name, deviceID string, volume float64) (*Player, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx == nil {
		return nil, ErrNotInitialized
	}
	if math.IsNaN(volume) || volume < 0 {
		volume = 0
	}
	if volume > 1 {
		volume = 1
	}
	return e.playSoundWithVolumeLocked(name, deviceID, true, &volume)
}

func (e *AudioPlayerEngine) playSoundWithVolumeLocked(name string, deviceID string, looping bool, initialVolume *float64) (*Player, error) {
	snd, ok := e.sounds[name]
	if !ok {
		return nil, fmt.Errorf("未预加载的音效: %s", name)
	}

	key := name + "|" + deviceID
	p, ok := e.cache[key]
	if ok {
		if initialVolume != nil {
			p.SetVolume(*initialVolume)
		}
		p.SetLoop(looping)
		return p, p.Replay()
	}

	var err error
	p, err = newPlayerFromSound(e.ctx, snd, deviceID)
	if err != nil {
		return nil, err
	}
	mv := math.Float32frombits(e.masterVolume.Load())
	if initialVolume != nil {
		mv = float32(*initialVolume)
	}
	p.SetVolume(float64(mv))
	mg := math.Float32frombits(e.masterGain.Load())
	p.SetGain(float64(mg))
	p.SetLoop(looping)
	e.players = append(e.players, p)
	e.cache[key] = p
	return p, p.Play()
}

// SetMasterVolume sets the volume for all current and future Players.
// factor 0.0 = silent, 1.0 = full volume.
func (e *AudioPlayerEngine) SetMasterVolume(factor float64) {
	if factor < 0 {
		factor = 0
	}
	if factor > 1.0 {
		factor = 1.0
	}
	e.masterVolume.Store(math.Float32bits(float32(factor)))
	e.mu.Lock()
	for _, p := range e.cache {
		p.SetVolume(factor)
	}
	e.mu.Unlock()
}

// MasterVolume returns the current master volume factor.
func (e *AudioPlayerEngine) MasterVolume() float64 {
	return float64(math.Float32frombits(e.masterVolume.Load()))
}

// SetMasterGain sets the gain for all current and future Players.
// factor 1.0 = unity, >1.0 amplifies. Clamped to [0, 5.0].
func (e *AudioPlayerEngine) SetMasterGain(factor float64) {
	if factor < 0 {
		factor = 0
	}
	if factor > 5.0 {
		factor = 5.0
	}
	e.masterGain.Store(math.Float32bits(float32(factor)))
	e.mu.Lock()
	for _, p := range e.cache {
		p.SetGain(factor)
	}
	e.mu.Unlock()
}

// MasterGain returns the current master gain factor.
func (e *AudioPlayerEngine) MasterGain() float64 {
	return float64(math.Float32frombits(e.masterGain.Load()))
}

// --- internal ---

func listDevicesWithCtx(ctx *malgo.AllocatedContext, kind malgo.DeviceType) ([]DeviceInfo, error) {
	devices, err := ctx.Devices(kind)
	if err != nil {
		return nil, err
	}
	result := make([]DeviceInfo, len(devices))
	for i, d := range devices {
		result[i] = DeviceInfo{
			ID:        d.ID.String(),
			Name:      d.Name(),
			IsDefault: d.IsDefault == 1,
		}
	}
	return result, nil
}

func loadSound(wavPath string) (*preloadedSound, error) {
	file, err := os.Open(wavPath)
	if err != nil {
		return nil, fmt.Errorf("打开WAV文件失败: %w", err)
	}
	defer file.Close()

	return loadSoundReader(file)
}

func loadSoundReader(file io.ReadSeeker) (*preloadedSound, error) {
	if file == nil {
		return nil, fmt.Errorf("WAV reader is required")
	}
	header, err := parseWAVHeader(file)
	if err != nil {
		return nil, fmt.Errorf("解析WAV头失败: %w", err)
	}
	if _, err := toMalgoFormat(header.bitsPerSample); err != nil {
		return nil, err
	}
	frameBytes := uint32(header.numChannels) * uint32(header.bitsPerSample/8)
	if frameBytes == 0 || header.dataSize%frameBytes != 0 {
		return nil, fmt.Errorf("WAV必须包含完整PCM帧")
	}

	pcmData, err := io.ReadAll(io.LimitReader(file, int64(header.dataSize)))
	if err != nil {
		return nil, fmt.Errorf("读取PCM数据失败: %w", err)
	}
	if uint32(len(pcmData)) != header.dataSize {
		return nil, fmt.Errorf("WAV数据不完整: 需要%d字节, 实际%d字节", header.dataSize, len(pcmData))
	}
	if header.numChannels == 0 || header.sampleRate == 0 || header.bitsPerSample == 0 || header.dataSize == 0 {
		return nil, fmt.Errorf("WAV格式无效")
	}

	return &preloadedSound{
		pcmData:       pcmData,
		numChannels:   header.numChannels,
		bitsPerSample: header.bitsPerSample,
		sampleRate:    header.sampleRate,
	}, nil
}
