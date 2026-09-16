

## Project overview

Go audio playback and capture library built on [miniaudio](https://miniaud.io/) via Go CGO bindings [malgo](https://github.com/gen2brain/malgo). Supports WAV (PCM) playback, playback/capture device enumeration, low-latency notification sound replay, and in-memory microphone recording for `tracklogic-asr`.

## Build & run

```bash
go build ./...              # build
go vet ./...                # lint
go run main.go              # list devices (default)
go run main.go list         # list devices
go run main.go play <wav> [deviceID]  # play a WAV file
```

`malgo` wraps the C library `miniaudio` → building requires a C compiler toolchain (GCC/MinGW on Windows, `build-essential` on Linux, Xcode CLI on macOS).

## Architecture

```
audio/
  engine.go      → shared context, device enumeration, playback cache
  player.go      → WAV playback and Player lifecycle
  recorder.go    → capture configuration, Recorder, and Recording
  errors.go      → public lifecycle errors
main.go          → playback CLI demo
```

### Package API

**Engine (shared context):**
```go
var engine audio.AudioEngine
if err := engine.Init(); err != nil {
    log.Fatal(err)
}
defer engine.Destroy()
```

**List input devices and record an ASR-ready segment:**

```go
devices, err := engine.ListCaptureDevices()
if err != nil {
    log.Fatal(err)
}
for _, device := range devices {
    fmt.Printf("%s  %s  default=%v\n", device.ID, device.Name, device.IsDefault)
}

recorder, err := engine.NewRecorder(audio.RecorderConfig{
    DeviceID:   "",    // empty selects the system default input
    SampleRate: 16000, // zero also defaults to 16 kHz
})
if err != nil {
    log.Fatal(err)
}
defer recorder.Close()

if err = recorder.Start(); err != nil {
    log.Fatal(err)
}
// Record until the application decides the utterance is complete.
recording, err := recorder.Stop()
if err != nil {
    log.Fatal(err)
}

// tracklogic-asr accepts this data directly:
result, err := recognizer.Transcribe(ctx, recording.Samples, recording.SampleRate, asr.Options{})
```

Capture output is always mono `float32` PCM. Each `Start` begins a fresh segment; `Stop` returns a copy that remains valid across later recordings. Because a complete segment is buffered in memory, callers should stop recordings at an application-defined duration.

**Cached playback:**

```go

engine.Preload("beep", "beep.wav")           // decode once, cache PCM
engine.PreloadWAV("embedded", reader)         // 从 embed.FS/io.ReadSeeker 解码
player, _ := engine.PlaySound("beep", "")    // 1st call: init device + play
<-player.Done()
player, _ = engine.PlaySound("beep", "")     // subsequent: Replay() — no device init
player, _ = engine.PlayLoop("engine_idle", "") // 循环播放（用于持续环境音）
player, _ = engine.PlayLoopWithVolume("engine_idle", "", 0) // v0.0.8：启动前设置单个循环的初始音量
```

`PlayLoopWithVolume` 在新建或重播缓存 Player 之前设置音量，不会创建额外引擎，也不会修改其他声部或引擎主音量。适用于先静音准备多层混音，再分别调整各层音量的场景。

**Key Player methods:**

- `Play()` — start (non-blocking)
- `Stop()` — pause, device stays initialized → can `Play()` again
- `Reset()` — rewind to beginning
- `Replay()` — Stop + Reset + Play in one call
- `SetLoop(enabled)` — 开关循环播放
- `Loop()` — 当前循环状态
- `SetPlaybackRate(rate)` — 设置实时播放速率（默认 1.0）
- `PlaybackRate()` — 当前播放速率
- `Close()` — stop + uninit device (one-shot players)
- `Done()` — channel that closes when playback reaches end

`ListDevices()` 每次调用都会重新枚举全部播放设备，并返回完整设备 ID、名称及
`IsDefault` 标识；将返回的 `ID` 传给 `PlaySound`/`PlayLoop` 即可指定输出设备。

### WAV support

Parses standard PCM WAV headers via `encoding/binary` (no external dependency). Supports 8/16/24/32-bit, mono/stereo, any sample rate. Non-PCM or compressed WAV formats return an error.

### Engine lifecycle

- `Engine.Destroy()` closes all Players and Recorders before freeing the malgo context
- The `"name|deviceID"` cache key means: same sound on different devices → separate cached Players
- Context is shared across Players and Recorders from the same Engine
