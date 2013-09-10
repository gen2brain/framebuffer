// This file is subject to a 1-clause BSD license.
// Its contents can be found in the enclosed LICENSE file.

package framebuffer

// Geometry represents a display's dimensions.
type Geometry struct {
	XRes  int // Visible horizontal resolution (in pixels)
	YRes  int // Visible vertical resolution (in pixels)
	XVRes int // Virtual horizontal resolution (in pixels)
	YVRes int // Virtual vertical resolution (in pixels)
	Depth int // Display depth (in bits per pixel)
}

// Timings represents a display's synchronization timings.
type Timings struct {
	Pixclock int // Length of one pixel (in picoseconds)
	Left     int // Left margin (in pixels)
	Right    int // Right margin (in pixels)
	Upper    int // Upper margin (in pixels)
	Lower    int // Lower margin (in pixels)
	HSLen    int // Horizontal sync length (in pixels)
	VSLen    int // Vertical sync length (in pixels)
}

// DisplayMode defines a single framebuffer display mode.
type DisplayMode struct {
	Geometry    Geometry    // Dimensions and bit depths.
	Timings     Timings     // Synchronization timings.
	Format      PixelFormat // Only valid with truecolor mode.
	Name        string      // Mode name.
	Nonstandard int         // Select nonstandard video mode.
	Sync        int         // SyncXXX bit flags defining synchronisation modes.
	VMode       int         // VModeXXX flags.
	Accelerated bool        // Anable or disable hardware text acceleration.
	Grayscale   bool        // Enable or disable graylevels instead of colors.
}

// Stride returns the width, in bytes, for a single row of pixels.
func (m *DisplayMode) Stride() int {
	return m.Geometry.XVRes * m.Format.Stride()
}

// HFreq returns the horizontal frequency.
func (m *DisplayMode) HFreq() float32 {
	return 1e12 / m.line()
}

// VFreq returns the vertical frequency.
func (m *DisplayMode) VFreq() float32 {
	return 1e12 / m.frame()
}

// Line length.
func (m *DisplayMode) line() float32 {
	htotal := m.Timings.Left + m.Geometry.XRes + m.Timings.Right + m.Timings.HSLen
	return float32(m.Timings.Pixclock * htotal)
}

// Frame length.
func (m *DisplayMode) frame() float32 {
	vtotal := m.Timings.Upper + m.Geometry.YRes + m.Timings.Lower + m.Timings.VSLen

	if (m.VMode & VModeInterlaced) != 0 {
		vtotal /= 2
	}

	if (m.VMode & VModeDouble) != 0 {
		vtotal *= 2
	}

	return float32(vtotal) * m.line()
}
