package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"

	"github.com/apexracing/tracklogic-audio/audio"
)

func main() {
	var engine audio.AudioPlayerEngine
	if err := engine.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "初始化音频引擎失败: %v\n", err)
		os.Exit(1)
	}
	defer engine.Destroy()

	if len(os.Args) < 2 {
		listDevices(&engine)
		return
	}

	switch os.Args[1] {
	case "list":
		listDevices(&engine)
	case "play":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "用法: go run main.go play <wav文件> [--gain <值>] [--device <设备ID>]")
			os.Exit(1)
		}
		wavPath, deviceID, gain := parsePlayArgs(os.Args[2:])
		playFile(&engine, wavPath, deviceID, gain)
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n", os.Args[1])
		fmt.Fprintln(os.Stderr, "可用命令: list, play")
		os.Exit(1)
	}
}

func parsePlayArgs(args []string) (wavPath, deviceID string, gain float64) {
	wavPath = args[0]
	deviceID = ""
	gain = 0 // 0 means use default (master gain, typically 1.0)

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--device":
			if i+1 < len(args) {
				deviceID = args[i+1]
				i++
			}
		case "--gain":
			if i+1 < len(args) {
				if v, err := strconv.ParseFloat(args[i+1], 64); err == nil {
					gain = v
				}
				i++
			}
		}
	}
	return
}

func listDevices(engine *audio.AudioPlayerEngine) {
	devices, err := engine.ListDevices()
	if err != nil {
		fmt.Fprintf(os.Stderr, "枚举设备失败: %v\n", err)
		os.Exit(1)
	}
	if len(devices) == 0 {
		fmt.Println("未找到任何播放设备")
		return
	}
	fmt.Println("音频播放设备:")
	for _, d := range devices {
		defaultMark := ""
		if d.IsDefault {
			defaultMark = " [默认]"
		}
		fmt.Printf("  %s  %s%s\n", d.ID, d.Name, defaultMark)
	}
}

func playFile(engine *audio.AudioPlayerEngine, wavPath, deviceID string, gain float64) {
	name := wavPath
	if err := engine.Preload(name, wavPath); err != nil {
		fmt.Fprintf(os.Stderr, "预加载失败: %v\n", err)
		os.Exit(1)
	}

	if gain > 0 {
		engine.SetMasterGain(gain)
		fmt.Printf("增益已设置为: %.2f\n", gain)
	}

	player, err := engine.PlaySound(name, deviceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "播放失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("正在播放,按 Ctrl+C 停止...")
	fmt.Printf("音量: %.2f, 增益: %.2f\n", engine.MasterVolume(), engine.MasterGain())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	select {
	case <-player.Done():
		fmt.Println("播放完毕")
	case <-sigCh:
		fmt.Println("播放已停止")
	}
}
