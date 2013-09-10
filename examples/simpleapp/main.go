// This file is subject to a 1-clause BSD license.
// Its contents can be found in the enclosed LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"github.com/jteeuwen/framebuffer"
	"image"
	"image/draw"
	_ "image/png"
	"os"
	"os/signal"
)

func main() {
	// Create a new framebuffer canvas.
	canvas, err := framebuffer.Open(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open: %v\n", err)
		return
	}

	// Ensure all resources are cleaned up properly before we exit.
	defer canvas.Close()

	mode, _ := canvas.CurrentMode()
	fmt.Fprintf(os.Stderr, "%+v\n", mode)

	// Fetch the framebuffer as a image/draw.Image implementation.
	// We can now use Go's image libraries to draw to it.
	fb, err := canvas.Image()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fb->image: %v\n", err)
		return
	}

	// Load the image we want to display.
	buf := bytes.NewBuffer(gophercolor_png())
	img, _, err := image.Decode(buf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Image decode: %v\n", err)
		return
	}

	// Draw the target image, centred on the buffer.
	fbb := fb.Bounds()
	imgb := img.Bounds()
	imgb = imgb.Add(image.Point{
		(fbb.Dx() / 2) - (imgb.Dx() / 2),
		(fbb.Dy() / 2) - (imgb.Dy() / 2),
	})

	draw.Draw(fb, imgb, img, image.ZP, draw.Src)

	wait() // Wait until an exit signal has been received.
}

// wait polls for exit signals.
func wait() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, os.Kill)

	for _ = range signals {
		return
	}
}
