package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"sync/atomic"

	"github.com/gen2brain/malgo"
)

const (
	riffChunkID    = "RIFF"
	waveFormat     = "WAVE"
	fmtChunkID     = "fmt "
	dataChunkID    = "data"
	pcmAudioFormat = 1
)

type wavHeader struct {
	sampleRate    uint32
	numChannels   uint16
	bitsPerSample uint16
	dataSize      uint32
	dataOffset    int64
}

// Player plays a WAV sound through an audio output device.
// Created via AudioPlayerEngine.PlaySound(). Can be replayed via Replay().
type playbackDevice interface {
	Start() error
	Stop() error
	IsStarted() bool
	Uninit()
	PlaybackInternalFormat() malgo.FormatType
	PlaybackInternalChannels() uint32
	PlaybackInternalSampleRate() uint32
}

type Player struct {
	device playbackDevice
	ctx    *malgo.AllocatedContext

	pcmData []byte
	// positionFrames is the playback cursor in source frames.
	positionFrames float64
	// gen is incremented on Reset/Replay, prevents stale callbacks.
	gen atomic.Int64

	// playbackRate uses atomic float32 bits, 1.0 means normal speed.
	playbackRate atomic.Uint32
	loop         atomic.Bool

	numChannels    int
	sampleRate     uint32
	bytesPerSample int
	bytesPerFrame  int
	sourceFrames   int

	volume   atomic.Uint32 // float32 bits, 1.0 = full volume
	gain     atomic.Uint32 // float32 bits, 1.0 = unity gain, >1.0 amplifies
	done     chan struct{}
	mu       sync.Mutex // playback state and done channel
	deviceMu sync.Mutex // device Start/Stop/Uninit; never hold mu while calling malgo
}

func newPlayerFromSound(ctx *malgo.AllocatedContext, snd *preloadedSound, deviceID string) (*Player, error) {
	format, err := toMalgoFormat(snd.bitsPerSample)
	if err != nil {
		return nil, err
	}

	deviceConfig := malgo.DefaultDeviceConfig(malgo.Playback)
	deviceConfig.Playback.Format = format
	deviceConfig.Playback.Channels = uint32(snd.numChannels)
	deviceConfig.SampleRate = snd.sampleRate

	if deviceID != "" {
		rawID, err := resolveDeviceID(ctx, malgo.Playback, deviceID)
		if err != nil {
			return nil, err
		}
		deviceConfig.Playback.DeviceID = rawID.Pointer()
	}

	numChannels := int(snd.numChannels)
	bytesPerSample := int(snd.bitsPerSample / 8)
	bytesPerFrame := numChannels * bytesPerSample
	sourceFrames := 0
	if bytesPerFrame > 0 {
		sourceFrames = len(snd.pcmData) / bytesPerFrame
	}

	p := &Player{
		ctx:            ctx,
		pcmData:        snd.pcmData,
		done:           make(chan struct{}),
		numChannels:    numChannels,
		sampleRate:     snd.sampleRate,
		bytesPerSample: bytesPerSample,
		bytesPerFrame:  bytesPerFrame,
		sourceFrames:   sourceFrames,
	}
	p.volume.Store(math.Float32bits(1.0))
	p.gain.Store(math.Float32bits(1.0))
	p.playbackRate.Store(math.Float32bits(1.0))

	callbacks := malgo.DeviceCallbacks{
		Data: p.dataCallback,
		Stop: p.stopCallback,
	}

	device, err := malgo.InitDevice(ctx.Context, deviceConfig, callbacks)
	if err != nil {
		return nil, fmt.Errorf("初始化播放设备失败: %w", err)
	}
	p.device = device

	return p, nil
}

// Play starts playback from the current position. Non-blocking.
func (p *Player) Play() error {
	p.deviceMu.Lock()
	defer p.deviceMu.Unlock()
	return p.device.Start()
}

// Stop pauses playback. The device stays initialized and can be restarted.
func (p *Player) Stop() error {
	p.deviceMu.Lock()
	defer p.deviceMu.Unlock()
	return p.device.Stop()
}

// Reset rewinds to the beginning so the Player can be replayed.
func (p *Player) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gen.Add(1)
	p.positionFrames = 0
	p.closeDoneLocked()
	p.done = make(chan struct{})
}

// Replay stops, resets, and starts playback from the beginning.
func (p *Player) Replay() error {
	p.deviceMu.Lock()
	defer p.deviceMu.Unlock()
	if p.device.IsStarted() {
		if err := p.device.Stop(); err != nil {
			return err
		}
	}
	p.mu.Lock()
	p.gen.Add(1)
	p.positionFrames = 0
	p.closeDoneLocked()
	p.done = make(chan struct{})
	p.mu.Unlock()
	return p.device.Start()
}

// Done returns a channel that is closed when playback finishes naturally.
func (p *Player) Done() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

func (p *Player) closeDoneLocked() {
	if p.done == nil {
		return
	}
	select {
	case <-p.done:
	default:
		close(p.done)
	}
}

// DeviceFormat returns the negotiated playback format details.
func (p *Player) DeviceFormat() (format malgo.FormatType, channels uint32, sampleRate uint32) {
	return p.device.PlaybackInternalFormat(),
		p.device.PlaybackInternalChannels(),
		p.device.PlaybackInternalSampleRate()
}

// SetVolume sets the playback volume factor (0.0 = silent, 1.0 = full).
func (p *Player) SetVolume(factor float64) {
	if factor < 0 {
		factor = 0
	}
	if factor > 1.0 {
		factor = 1.0
	}
	p.volume.Store(math.Float32bits(float32(factor)))
}

// Volume returns the current playback volume factor.
func (p *Player) Volume() float64 {
	return float64(math.Float32frombits(p.volume.Load()))
}

// SetGain sets the gain factor. 1.0 = unity, >1.0 amplifies.
// Clamped to [0, 5.0].
func (p *Player) SetGain(factor float64) {
	if factor < 0 {
		factor = 0
	}
	if factor > 5.0 {
		factor = 5.0
	}
	p.gain.Store(math.Float32bits(float32(factor)))
}

// Gain returns the current gain factor.
func (p *Player) Gain() float64 {
	return float64(math.Float32frombits(p.gain.Load()))
}

// SetLoop sets whether the player should keep looping.
func (p *Player) SetLoop(enabled bool) {
	p.loop.Store(enabled)
}

// Loop reports whether loop playback is enabled.
func (p *Player) Loop() bool {
	return p.loop.Load()
}

// SetPlaybackRate updates playback rate and pitch.
//
// Clamps to [0.25, 4.0] for safety.
func (p *Player) SetPlaybackRate(rate float64) {
	if rate < 0.25 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		rate = 0.25
	}
	if rate > 4.0 {
		rate = 4.0
	}
	p.playbackRate.Store(math.Float32bits(float32(rate)))
}

// PlaybackRate returns current playback rate.
func (p *Player) PlaybackRate() float64 {
	return float64(math.Float32frombits(p.playbackRate.Load()))
}

// --- internal ---

func (p *Player) dataCallback(pOutput, _ []byte, frameCount uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()

	gen := p.gen.Load()
	loop := p.loop.Load()
	rate := p.playbackRateValue()
	if len(pOutput) == 0 {
		return
	}

	maxFramesByBuffer := len(pOutput) / maxInt(1, p.bytesPerFrame)
	requestedFrames := int(frameCount)
	if requestedFrames > maxFramesByBuffer {
		requestedFrames = maxFramesByBuffer
	}
	if requestedFrames <= 0 {
		return
	}
	requestedBytes := requestedFrames * p.bytesPerFrame

	if p.sourceFrames <= 0 || p.bytesPerFrame == 0 {
		for i := 0; i < requestedBytes; i++ {
			pOutput[i] = silenceByte(p.bytesPerSample)
		}
		if p.gen.Load() == gen {
			select {
			case <-p.done:
			default:
				close(p.done)
			}
		}
		for i := requestedBytes; i < len(pOutput); i++ {
			pOutput[i] = silenceByte(p.bytesPerSample)
		}
		return
	}

	vol := math.Float32frombits(p.volume.Load())
	gn := math.Float32frombits(p.gain.Load())
	effective := float64(vol) * float64(gn)

	// Fast path for unchanged playback state.
	if !loop && rate == 1.0 && effective == 1.0 && p.positionFrames == math.Trunc(p.positionFrames) {
		posByte := int(p.positionFrames * float64(p.bytesPerFrame))
		if posByte >= len(p.pcmData) {
			for i := 0; i < requestedBytes; i++ {
				pOutput[i] = silenceByte(p.bytesPerSample)
			}
			if p.gen.Load() == gen {
				select {
				case <-p.done:
				default:
					close(p.done)
				}
			}
			for i := requestedBytes; i < len(pOutput); i++ {
				pOutput[i] = silenceByte(p.bytesPerSample)
			}
			return
		}

		n := copy(pOutput[:requestedBytes], p.pcmData[posByte:])
		p.positionFrames += float64(n) / float64(p.bytesPerFrame)
		for i := n; i < requestedBytes; i++ {
			pOutput[i] = silenceByte(p.bytesPerSample)
		}
		if p.positionFrames >= float64(p.sourceFrames) && p.gen.Load() == gen {
			select {
			case <-p.done:
			default:
				close(p.done)
			}
		}
		for i := requestedBytes; i < len(pOutput); i++ {
			pOutput[i] = silenceByte(p.bytesPerSample)
		}
		return
	}

	framesWritten := 0
	for i := 0; i < requestedFrames; i++ {
		for p.positionFrames >= float64(p.sourceFrames) {
			if !loop {
				p.positionFrames = float64(p.sourceFrames)
				break
			}
			p.positionFrames -= float64(p.sourceFrames)
		}

		if !loop && p.positionFrames >= float64(p.sourceFrames) {
			break
		}

		baseFrame := int(p.positionFrames)
		frac := p.positionFrames - float64(baseFrame)
		nextFrame := baseFrame + 1
		if nextFrame >= p.sourceFrames {
			if loop {
				nextFrame = 0
			} else {
				nextFrame = baseFrame
			}
		}

		frameBaseOut := i * p.bytesPerFrame
		for ch := 0; ch < p.numChannels; ch++ {
			sample0 := readSampleAtFrame(p.pcmData, baseFrame, ch, p.numChannels, p.bytesPerSample)
			sample1 := sample0
			if frac > 0 {
				sample1 = readSampleAtFrame(p.pcmData, nextFrame, ch, p.numChannels, p.bytesPerSample)
			}
			sample := sample0*(1-frac) + sample1*frac
			sample *= effective
			writeSample(pOutput[frameBaseOut+ch*p.bytesPerSample:frameBaseOut+(ch+1)*p.bytesPerSample], sample, p.bytesPerSample)
		}

		framesWritten++
		p.positionFrames += rate
	}

	for i := framesWritten * p.bytesPerFrame; i < requestedBytes; i++ {
		pOutput[i] = silenceByte(p.bytesPerSample)
	}
	for i := requestedBytes; i < len(pOutput); i++ {
		pOutput[i] = silenceByte(p.bytesPerSample)
	}

	if !loop && p.positionFrames >= float64(p.sourceFrames) && p.gen.Load() == gen {
		select {
		case <-p.done:
		default:
			close(p.done)
		}
	}
}

func (p *Player) playbackRateValue() float64 {
	rate := float64(math.Float32frombits(p.playbackRate.Load()))
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		rate = 1.0
	}
	return rate
}

func (p *Player) stopCallback() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeDoneLocked()
}

func readSampleAtFrame(pcm []byte, frame int, channel int, channels int, bytesPerSample int) float64 {
	offset := (frame*channels + channel) * bytesPerSample
	return readSample(pcm, offset, bytesPerSample)
}

func readSample(pcm []byte, offset int, bytesPerSample int) float64 {
	if offset < 0 || offset+bytesPerSample > len(pcm) {
		return 0
	}
	switch bytesPerSample {
	case 1:
		return float64(int32(int16(pcm[offset])-128)) / 127.0
	case 2:
		sample := int16(binary.LittleEndian.Uint16(pcm[offset : offset+2]))
		return float64(sample) / 32768.0
	case 3:
		raw := uint32(pcm[offset]) | uint32(pcm[offset+1])<<8 | uint32(pcm[offset+2])<<16
		if raw&0x800000 != 0 {
			raw |= 0xFF000000
		}
		sample := int32(raw)
		return float64(sample) / 8388608.0
	case 4:
		sample := int32(binary.LittleEndian.Uint32(pcm[offset : offset+4]))
		return float64(sample) / 2147483648.0
	default:
		return 0
	}
}

func writeSample(dst []byte, sample float64, bytesPerSample int) {
	switch bytesPerSample {
	case 1:
		v := int(math.Round(sample*127.0 + 128.0))
		if v > 255 {
			v = 255
		} else if v < 0 {
			v = 0
		}
		dst[0] = byte(v)
	case 2:
		s := int64(math.Round(sample * 32767.0))
		if s > 32767 {
			s = 32767
		} else if s < -32768 {
			s = -32768
		}
		binary.LittleEndian.PutUint16(dst[:2], uint16(s))
	case 3:
		s := int64(math.Round(sample * 8388607.0))
		if s > 8388607 {
			s = 8388607
		} else if s < -8388608 {
			s = -8388608
		}
		u := uint32(s)
		dst[0] = byte(u)
		dst[1] = byte(u >> 8)
		dst[2] = byte(u >> 16)
	case 4:
		s := int64(math.Round(sample * 2147483647.0))
		if s > 2147483647 {
			s = 2147483647
		} else if s < -2147483648 {
			s = -2147483648
		}
		binary.LittleEndian.PutUint32(dst[:4], uint32(s))
	}
}

func parseWAVHeader(r io.ReadSeeker) (*wavHeader, error) {
	var riff [4]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return nil, errors.New("无法读取RIFF标识")
	}
	if string(riff[:]) != riffChunkID {
		return nil, errors.New("不是有效的RIFF文件")
	}

	r.Seek(4, io.SeekCurrent) // skip file size

	var wave [4]byte
	if _, err := io.ReadFull(r, wave[:]); err != nil {
		return nil, errors.New("无法读取WAVE标识")
	}
	if string(wave[:]) != waveFormat {
		return nil, errors.New("不是WAV文件")
	}

	var hdr wavHeader
	foundFmt := false

	for {
		var chunkID [4]byte
		_, err := io.ReadFull(r, chunkID[:])
		if err != nil {
			break
		}

		var chunkSize uint32
		if err := binary.Read(r, binary.LittleEndian, &chunkSize); err != nil {
			return nil, fmt.Errorf("读取chunk大小失败: %w", err)
		}

		switch string(chunkID[:]) {
		case fmtChunkID:
			if err := readFmtChunk(r, chunkSize, &hdr); err != nil {
				return nil, err
			}
			foundFmt = true

		case dataChunkID:
			if !foundFmt {
				return nil, errors.New("data chunk出现在fmt chunk之前")
			}
			hdr.dataSize = chunkSize
			offset, _ := r.Seek(0, io.SeekCurrent)
			hdr.dataOffset = offset
			return &hdr, nil

		default:
			r.Seek(int64(chunkSize), io.SeekCurrent)
		}
	}

	return nil, errors.New("未找到fmt或data chunk")
}

func readFmtChunk(r io.Reader, chunkSize uint32, hdr *wavHeader) error {
	if chunkSize < 16 {
		return errors.New("fmt chunk太小")
	}

	var audioFormat uint16
	var byteRate uint32
	var blockAlign uint16
	var extraSize uint16

	if err := binary.Read(r, binary.LittleEndian, &audioFormat); err != nil {
		return err
	}
	if audioFormat != pcmAudioFormat {
		return fmt.Errorf("不支持的音频格式: %d (仅支持PCM)", audioFormat)
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr.numChannels); err != nil {
		return err
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr.sampleRate); err != nil {
		return err
	}
	if err := binary.Read(r, binary.LittleEndian, &byteRate); err != nil {
		return err
	}
	if err := binary.Read(r, binary.LittleEndian, &blockAlign); err != nil {
		return err
	}
	_ = byteRate
	_ = blockAlign
	if err := binary.Read(r, binary.LittleEndian, &hdr.bitsPerSample); err != nil {
		return err
	}

	if chunkSize > 16 {
		extraBytes := chunkSize - 16
		if chunkSize >= 18 {
			if err := binary.Read(r, binary.LittleEndian, &extraSize); err != nil {
				return err
			}
			extraBytes -= 2
		}
		discard := make([]byte, extraBytes)
		if _, err := io.ReadFull(r, discard); err != nil {
			return err
		}
	}
	return nil
}

func toMalgoFormat(bitsPerSample uint16) (malgo.FormatType, error) {
	switch bitsPerSample {
	case 8:
		return malgo.FormatU8, nil
	case 16:
		return malgo.FormatS16, nil
	case 24:
		return malgo.FormatS24, nil
	case 32:
		return malgo.FormatS32, nil
	default:
		return malgo.FormatUnknown, fmt.Errorf("不支持的位深度: %d", bitsPerSample)
	}
}

func resolveDeviceID(ctx *malgo.AllocatedContext, kind malgo.DeviceType, hexID string) (malgo.DeviceID, error) {
	devices, err := ctx.Devices(kind)
	if err != nil {
		return malgo.DeviceID{}, err
	}
	for _, d := range devices {
		if d.ID.String() == hexID {
			return d.ID, nil
		}
	}
	return malgo.DeviceID{}, fmt.Errorf("未找到设备ID: %s", hexID)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func silenceByte(bytesPerSample int) byte {
	if bytesPerSample == 1 {
		return 128
	}
	return 0
}
