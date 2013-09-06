// This file is subject to a 1-clause BSD license.
// Its contents can be found in the enclosed LICENSE file.

package framebuffer

// DisplayMode defines a supported display mode.
type DisplayMode struct {
	Name   string
	Format PixelFormat

	XRes  int // Visible resolution.
	YRes  int
	XVRes int // Virtual resolution (viewport).
	YVRes int
	Bpp   int // Bit depth.

	// Timing: All values in pixclocks, except pixclock.
	Pixclock int // Pixel clock in picoseconds.
	Left     int // Time in pixclocks from sync to picture.
	Right    int // Time in pixclocks from picture to sync.
	Upper    int // Time in pixclocks from sync to picture.
	Lower    int // Time in pixclocks from picture to sync.
	HSync    int // Time in pixclocks of horizontal sync.
	VSync    int // Time in pixclocks of vertical sync.
	Sync     int // See SyncXXXX values.
	VMode    int // See VModeXXXX values.
}

// Stride returns the width, in bytes, for a single row of pixels.
func (dm *DisplayMode) Stride() int {
	return dm.XVRes * dm.Format.Stride()
}
