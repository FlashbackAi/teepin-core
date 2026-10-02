// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package modelprobe

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// The test inputs are generated, not bundled: nothing to ship or go stale, and
// each attempt can use a different answer so a model cannot pass by guessing.

type namedColour struct {
	name string
	rgb  color.RGBA
}

var colours = []namedColour{
	{"red", color.RGBA{220, 30, 30, 255}},
	{"blue", color.RGBA{30, 60, 220, 255}},
	{"green", color.RGBA{30, 170, 60, 255}},
	{"yellow", color.RGBA{240, 220, 30, 255}},
	{"black", color.RGBA{0, 0, 0, 255}},
	{"white", color.RGBA{255, 255, 255, 255}},
}

// twoToneImage returns a PNG, left half one colour and right half another, as a
// data URL.
func twoToneImage(left, right namedColour) string {
	const w, h = 96, 64
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < w/2 {
				img.Set(x, y, left.rgb)
			} else {
				img.Set(x, y, right.rgb)
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// beepsWAV returns a 16 kHz mono WAV of n short 880 Hz beeps separated by
// silence, base64 encoded.
func beepsWAV(n int) string {
	const rate = 16000
	const beepMs, gapMs = 300, 350
	var samples []int16
	silence := func(ms int) {
		samples = append(samples, make([]int16, rate*ms/1000)...)
	}
	silence(200)
	for i := 0; i < n; i++ {
		count := rate * beepMs / 1000
		fade := rate / 100
		for s := 0; s < count; s++ {
			// A short fade in and out avoids clicks that sound like extra beeps.
			env := 1.0
			if s < fade {
				env = float64(s) / float64(fade)
			} else if s > count-fade {
				env = float64(count-s) / float64(fade)
			}
			samples = append(samples, int16(env*0.6*32767*math.Sin(2*math.Pi*880*float64(s)/rate)))
		}
		silence(gapMs)
	}

	var buf bytes.Buffer
	dataLen := uint32(len(samples) * 2)
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, 36+dataLen)
	buf.WriteString("WAVEfmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // mono
	_ = binary.Write(&buf, binary.LittleEndian, uint32(rate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(rate*2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, dataLen)
	_ = binary.Write(&buf, binary.LittleEndian, samples)
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
