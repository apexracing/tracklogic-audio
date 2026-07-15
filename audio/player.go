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
type Player struct {
	device *malgo.Device
	ctx    *malgo.AllocatedContext

	pcmData        []byte
	pos            int
	gen            atomic.Int64 // incremented on Reset/Replay, prevents stale callbacks
	bytesPerSample int          // 1, 2, 3, or 4

	volume atomic.Uint32 // float32 bits, 1.0 = full volume
	gain   atomic.Uint32 // float32 bits, 1.0 = unity gain, >1.0 amplifies
	done   chan struct{}
	mu     sync.Mutex
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

	p := &Player{
		ctx:            ctx,
		pcmData:        snd.pcmData,
		done:           make(chan struct{}),
		bytesPerSample: int(snd.bitsPerSample / 8),
	}
	p.volume.Store(math.Float32bits(1.0))
	p.gain.Store(math.Float32bits(1.0))

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
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.device.Start()
}

// Stop pauses playback. The device stays initialized and can be restarted.
func (p *Player) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.device.Stop()
}

// Reset rewinds to the beginning so the Player can be replayed.
func (p *Player) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gen.Add(1)
	p.pos = 0
	p.done = make(chan struct{})
}

// Replay stops, resets, and starts playback from the beginning.
func (p *Player) Replay() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.device.IsStarted() {
		if err := p.device.Stop(); err != nil {
			return err
		}
	}
	p.gen.Add(1)
	p.pos = 0
	p.done = make(chan struct{})
	return p.device.Start()
}

// Done returns a channel that is closed when playback finishes naturally.
func (p *Player) Done() <-chan struct{} {
	return p.done
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

// --- internal ---

func (p *Player) dataCallback(pOutput, _ []byte, frameCount uint32) {
	gen := p.gen.Load()
	if p.pos >= len(p.pcmData) {
		for i := range pOutput {
			pOutput[i] = 0
		}
		if p.gen.Load() == gen {
			select {
			case <-p.done:
			default:
				close(p.done)
			}
		}
		return
	}

	n := copy(pOutput, p.pcmData[p.pos:])
	p.pos += n

	vol := math.Float32frombits(p.volume.Load())
	gn := math.Float32frombits(p.gain.Load())
	effective := float64(vol) * float64(gn)
	if effective != 1.0 {
		switch p.bytesPerSample {
		case 1: // u8: unsigned 8-bit, center=128
			for i := 0; i < n; i++ {
				centered := int32(pOutput[i]) - 128
				scaled := int32(float64(centered) * effective)
				if scaled > 127 {
					scaled = 127
				} else if scaled < -128 {
					scaled = -128
				}
				pOutput[i] = uint8(scaled + 128)
			}
		case 2: // s16: signed 16-bit little-endian
			for i := 0; i+1 < n; i += 2 {
				sample := int16(binary.LittleEndian.Uint16(pOutput[i : i+2]))
				scaled := int32(float64(sample) * effective)
				if scaled > 32767 {
					scaled = 32767
				} else if scaled < -32768 {
					scaled = -32768
				}
				binary.LittleEndian.PutUint16(pOutput[i:i+2], uint16(scaled))
			}
		case 3: // s24: signed 24-bit little-endian
			for i := 0; i+2 < n; i += 3 {
				raw := uint32(pOutput[i]) | uint32(pOutput[i+1])<<8 | uint32(pOutput[i+2])<<16
				if raw&0x800000 != 0 {
					raw |= 0xFF000000
				}
				sample := int32(raw)
				scaled := int64(float64(sample) * effective)
				if scaled > 8388607 {
					scaled = 8388607
				} else if scaled < -8388608 {
					scaled = -8388608
				}
				v := uint32(scaled)
				pOutput[i] = byte(v)
				pOutput[i+1] = byte(v >> 8)
				pOutput[i+2] = byte(v >> 16)
			}
		case 4: // s32: signed 32-bit little-endian
			for i := 0; i+3 < n; i += 4 {
				sample := int32(binary.LittleEndian.Uint32(pOutput[i : i+4]))
				scaled := int64(float64(sample) * effective)
				if scaled > 2147483647 {
					scaled = 2147483647
				} else if scaled < -2147483648 {
					scaled = -2147483648
				}
				binary.LittleEndian.PutUint32(pOutput[i:i+4], uint32(scaled))
			}
		}
	}

	for i := n; i < len(pOutput); i++ {
		pOutput[i] = 0
	}
	if p.pos >= len(p.pcmData) {
		if p.gen.Load() == gen {
			select {
			case <-p.done:
			default:
				close(p.done)
			}
		}
	}
}

func (p *Player) stopCallback() {
	select {
	case <-p.done:
	default:
		close(p.done)
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
