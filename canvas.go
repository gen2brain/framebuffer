// This file is subject to a 1-clause BSD license.
// Its contents can be found in the enclosed LICENSE file.

package framebuffer

import (
	"bufio"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// Format strings for device locations.
// Values depend on whether we are dealing with DevFS or not.
var (
	fb0   = "/dev/fb0"
	fbnr  = "/dev/fb%d"
	ttynr = "/dev/tty%d"
)

func init() {
	_, err := os.Lstat("/dev/.devfsd")
	if err == nil {
		fb0 = "/dev/fb/0"
		fbnr = "/dev/fb/%d"
		ttynr = "/dev/vc/%d"
	}
}

// Linux Framebuffer implementation.
type Canvas struct {
	// Backup storage.
	// These hold the initial system state, which will be restored once we shut down.
	orig_fi    fb_fix_screeninfo // Fixed buffer settings.
	orig_vi    fb_var_screeninfo // Variable buffer settings.
	orig_r     [256]uint16       // Palette red channel.
	orig_g     [256]uint16       // Palette green channel.
	orig_b     [256]uint16       // Palette blue channel.
	orig_a     [256]uint16       // Palette transparent channel.
	orig_vt    vt_mode           // Virtual terminal mode.
	orig_vt_no int               // Virtual terminal number.
	orig_kd    int               // KD mode.

	// Framebuffer state and access bits.
	fd           *os.File // Framebuffer file descriptor.
	tty          *os.File // Current tty.
	mem          []byte   // mmap'd memory.
	dev          string   // name of the device we are using.
	switch_state int      // Current switch state.

	// pre-allocated scratchpad values.
	zero  []byte
	tmp_r [256]uint16
	tmp_g [256]uint16
	tmp_b [256]uint16
	tmp_a [256]uint16
}

// New creates a new, uninitialized canvas.
func New() *Canvas {
	c := new(Canvas)
	c.tty = os.Stdout
	c.orig_vt_no = 0
	c.switch_state = _FB_ACTIVE
	go c.pollSignals()
	return c
}

// Open opens the framebuffer with the given display mode.
//
// If mode is nil, the default framebuffer mode is used.
//
// The framebuffer is usually initialized to a specific display mode by the
// kernel itself. While this library supplies the means to alter the current
// display mode, this may not always have any effect as a driver can
// choose to ignore your requested values. Besides that, it is generally
// considered safer to use the external `fbset` command for this purpose.
//
// Video modes for the framebuffer require very precise timing values to
// be supplied along with any desired resolution. Doing this incorrectly
// can damage the display. Refer to Canvas.Modes() and Canvas.FindMode()
// for more information. Canvas.CurrentMode() can be used to see which
// mode is actually being used.
func (c *Canvas) Open(dm *DisplayMode) (err error) {
	defer func() {
		// Ensure resources are properly cleaned up when things go booboo.
		if err != nil {
			c.Close()
		}
	}()

	// Get VT state
	var vts vt_stat
	err = ioctl(c.tty.Fd(), _VT_GETSTATE, unsafe.Pointer(&vts))
	if err != nil {
		return
	}

	// Determine which framebuffer to use.
	c.dev = os.Getenv("FRAMEBUFFER")
	if len(c.dev) == 0 {
		var c2m fb_con2fbmap
		var fd *os.File

		fd, err = os.OpenFile(fb0, os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("open %q: %v", fb0, err)
		}

		c2m.console = uint32(vts.v_active)
		err = ioctl(fd.Fd(), _IOGET_CON2FBMAP, unsafe.Pointer(&c2m))
		fd.Close()

		if err != nil {
			return
		}

		c.dev = fmt.Sprintf(fbnr, c2m.framebuffer)
	}

	// Open the frame buffer.
	c.fd, err = os.OpenFile(c.dev, os.O_RDWR, 0)
	if err != nil {
		return
	}

	// Fetch original fixed buffer information.
	// This will never be changed, but we need the information
	// in various places.
	err = ioctl(c.fd.Fd(), _IOGET_FSCREENINFO, unsafe.Pointer(&c.orig_fi))
	if err != nil {
		return
	}

	// Fetch original variable information.
	err = ioctl(c.fd.Fd(), _IOGET_VSCREENINFO, unsafe.Pointer(&c.orig_vi))
	if err != nil {
		return
	}

	// Fetch original color palette if applicable.
	if c.orig_vi.bits_per_pixel == 8 || c.orig_fi.visual == _VISUAL_DIRECTCOLOR {
		var cm fb_cmap
		cm.start = 0
		cm.len = 256
		cm.red = unsafe.Pointer(&c.orig_r[0])
		cm.green = unsafe.Pointer(&c.orig_g[0])
		cm.blue = unsafe.Pointer(&c.orig_b[0])
		cm.transp = unsafe.Pointer(&c.orig_a[0])

		err = ioctl(c.fd.Fd(), _IOGET_CMAP, unsafe.Pointer(&cm))
		if err != nil {
			return
		}
	}

	// Get KD mode
	err = ioctl(c.tty.Fd(), _KDGETMODE, unsafe.Pointer(&c.orig_kd))
	if err != nil {
		return
	}

	// Get original vt mode
	err = ioctl(c.tty.Fd(), _VT_GETMODE, unsafe.Pointer(&c.orig_vt))
	if err != nil {
		return
	}

	// Set display mode.
	err = c.setMode(dm)
	if err != nil {
		return
	}

	// Fetch original fixed buffer information (again).
	err = ioctl(c.fd.Fd(), _IOGET_FSCREENINFO, unsafe.Pointer(&c.orig_fi))
	if err != nil {
		return
	}

	// Ensure we are in PACKED_PIXELS mode. Others are useless to us.
	if c.orig_fi.typ != _TYPE_PACKED_PIXELS {
		return errors.New("Canvas.Open: Framebuffer is not in PACKED PIXELS mode. Unable to continue.")
	}

	// If we have a non-standard pixel format, we can't continue.
	if c.orig_vi.nonstd != 0 {
		return errors.New("Canvas.Open: Framebuffer uses a non-standard pixel format. This is not supported.")
	}

	// mmap the buffer's memory.
	c.mem, err = syscall.Mmap(int(c.fd.Fd()), 0, int(c.orig_fi.smemlen),
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return errors.New("Canvas.Open: Mmap failed")
	}

	// Create pre-allocated zero-memory.
	// This is used to do fast screen clears.
	c.zero = make([]byte, len(c.mem))

	// Move viewport to top-left corner.
	if c.orig_vi.xoffset != 0 || c.orig_vi.yoffset != 0 {
		vi := c.orig_vi.Copy()
		vi.xoffset = 0
		vi.yoffset = 0

		err = ioctl(c.fd.Fd(), _IOPAN_DISPLAY, unsafe.Pointer(vi))
		if err != nil {
			return
		}
	}

	// Switch terminal to graphics mode.
	err = ioctl(c.tty.Fd(), _KDSETMODE, _KD_GRAPHICS)
	if err != nil {
		return
	}

	// Activate the given tty.
	err = c.activateCurrent(c.tty)
	if err != nil {
		return
	}

	// Clear screen
	c.Clear()
	return
}

// Close closes the framebuffer and cleans up its resources.
func (c *Canvas) Close() (err error) {
	if c.mem != nil {
		syscall.Munmap(c.mem)
		c.mem = nil
	}

	if c.fd != nil {
		// Restore original framebuffer settings.
		err = ioctl(c.fd.Fd(), _IOPUT_VSCREENINFO, unsafe.Pointer(&c.orig_vi))
		if err != nil {
			goto skip_fd
		}

		// Restore original color palette.
		if c.orig_vi.bits_per_pixel == 8 || c.orig_fi.visual == _VISUAL_DIRECTCOLOR {
			var cm fb_cmap
			cm.start = 0
			cm.len = 256
			cm.red = unsafe.Pointer(&c.orig_r[0])
			cm.green = unsafe.Pointer(&c.orig_g[0])
			cm.blue = unsafe.Pointer(&c.orig_b[0])
			cm.transp = unsafe.Pointer(&c.orig_a[0])

			err = ioctl(c.fd.Fd(), _IOPUT_CMAP, unsafe.Pointer(&cm))
		}

	skip_fd:
		c.fd.Close()
		c.fd = nil
	}

	if c.tty != nil {
		err = ioctl(c.tty.Fd(), _KDSETMODE, c.orig_kd)
		if err != nil {
			goto skip_tty
		}

		err = ioctl(c.tty.Fd(), _VT_SETMODE, unsafe.Pointer(&c.orig_vt))
		if err != nil {
			goto skip_tty
		}

		if c.orig_vt_no > 0 {
			err = ioctl(c.tty.Fd(), _VT_ACTIVATE, c.orig_vt_no)
			if err != nil {
				goto skip_tty
			}

			err = ioctl(c.tty.Fd(), _VT_WAITACTIVE, c.orig_vt_no)
		}

	skip_tty:
		//c.tty.Close() // Don't close stdout
		c.tty = nil
	}

	return
}

// Image returns the pixel buffer as a draw.Image instance.
// Returns nil if something went wrong.
func (c *Canvas) Image() (draw.Image, error) {
	mode, err := c.CurrentMode()
	if err != nil {
		return nil, err
	}

	p := c.mem
	s := mode.Stride()
	r := image.Rect(0, 0, mode.XVRes, mode.YVRes)

	// Find out which image type we should be returning.
	// This depends on the current pixel format.
	switch mode.Format.Type() {
	case PF_RGBA:
		return &image.RGBA{Pix: p, Stride: s, Rect: r}, nil

	case PF_BGRA:
		return &BGRA{Pix: p, Stride: s, Rect: r}, nil

	case PF_RGB_555:
		return &RGB555{Pix: p, Stride: s, Rect: r}, nil

	case PF_RGB_565:
		return &RGB565{Pix: p, Stride: s, Rect: r}, nil

	case PF_BGR_555:
		return &BGR555{Pix: p, Stride: s, Rect: r}, nil

	case PF_BGR_565:
		return &BGR565{Pix: p, Stride: s, Rect: r}, nil

	case PF_INDEXED:
		return &image.Alpha{Pix: p, Stride: s, Rect: r}, nil
	}

	return nil, fmt.Errorf("Unsupported pixelformat: %+v", mode.Format)
}

// Clear clears (zeroes) the framebuffer memory.
func (c *Canvas) Clear() {
	copy(c.mem, c.zero)
}

// Accelerated returns true if the framebuffer
// currently supports hardware acceleration.
func (c *Canvas) Accelerated() bool {
	return c.orig_fi.accel != _ACCEL_NONE
}

// Buffer provides direct access to the entire memory-mapped pixel buffer.
func (c *Canvas) Buffer() []byte {
	return c.mem
}

// setMode sets the given display mode.
// If the mode is nil, this returns without error;
// the call is simply ignored.
func (c *Canvas) setMode(dm *DisplayMode) error {
	if dm == nil {
		return nil
	}

	var v fb_var_screeninfo

	err := ioctl(c.fd.Fd(), _IOGET_VSCREENINFO, unsafe.Pointer(&v))
	if err != nil {
		return err
	}

	v.xres = uint32(dm.XRes)
	v.yres = uint32(dm.YRes)
	v.xres_virtual = uint32(dm.XVRes)
	v.yres_virtual = uint32(dm.YVRes)
	v.bits_per_pixel = uint32(dm.Bpp)
	v.pixclock = uint32(dm.Pixclock)
	v.left_margin = uint32(dm.Left)
	v.right_margin = uint32(dm.Right)
	v.upper_margin = uint32(dm.Upper)
	v.lower_margin = uint32(dm.Lower)
	v.hsync_len = uint32(dm.HSync)
	v.vsync_len = uint32(dm.VSync)
	v.sync = uint32(dm.Sync)
	v.vmode = uint32(dm.VMode)

	pf := dm.Format
	v.red.length = uint32(pf.RedBits)
	v.red.offset = uint32(pf.RedShift)
	v.red.msb_right = 1

	v.green.length = uint32(pf.GreenBits)
	v.green.offset = uint32(pf.GreenShift)
	v.green.msb_right = 1

	v.blue.length = uint32(pf.BlueBits)
	v.blue.offset = uint32(pf.BlueShift)
	v.blue.msb_right = 1

	v.transparent.length = uint32(pf.AlphaBits)
	v.transparent.offset = uint32(pf.AlphaShift)
	v.transparent.msb_right = 1

	v.xoffset = 0
	v.yoffset = 0

	return ioctl(c.fd.Fd(), _IOPUT_VSCREENINFO, unsafe.Pointer(&v))
}

// CurrentMode returns the current framebuffer display mode.
func (c *Canvas) CurrentMode() (*DisplayMode, error) {
	var v fb_var_screeninfo
	var dm DisplayMode

	if ioctl(c.fd.Fd(), _IOGET_VSCREENINFO, unsafe.Pointer(&v)) != nil {
		return nil, errors.New("Canvas.CurrentMode failed.")
	}

	dm.XRes = int(v.xres)
	dm.YRes = int(v.yres)
	dm.XVRes = int(v.xres_virtual)
	dm.YVRes = int(v.yres_virtual)
	dm.Bpp = int(v.bits_per_pixel)
	dm.Pixclock = int(v.pixclock)
	dm.Left = int(v.left_margin)
	dm.Right = int(v.right_margin)
	dm.Upper = int(v.upper_margin)
	dm.Lower = int(v.lower_margin)
	dm.HSync = int(v.hsync_len)
	dm.VSync = int(v.vsync_len)
	dm.Sync = int(v.sync)
	dm.VMode = int(v.vmode)

	var pf PixelFormat
	pf.RedBits = uint8(v.red.length)
	pf.RedShift = uint8(v.red.offset)
	pf.GreenBits = uint8(v.green.length)
	pf.GreenShift = uint8(v.green.offset)
	pf.BlueBits = uint8(v.blue.length)
	pf.BlueShift = uint8(v.blue.offset)
	pf.AlphaBits = uint8(v.transparent.length)
	pf.AlphaShift = uint8(v.transparent.offset)
	dm.Format = pf

	return &dm, nil
}

// FindMode finds the display mode with the given name.
// Returns nil if it does not exist.
//
// The external `fbset` tool comes with a set of default modes
// which are stored in the file `/etc/fb.modes`. We read this file
// and extract the set of video modes from it. These modes each have
// a name by which they can be identified. When supplying a new
// mode to this function, it should come in the form of this name.
// For example: "1600x1200-76".
//
// New video modes can be added to the `/etc/fb.modes` file.
func (c *Canvas) FindMode(name string) *DisplayMode {
	modes, err := c.Modes()
	if err != nil {
		return nil
	}

	for _, m := range modes {
		if strings.EqualFold(m.Name, name) {
			return m
		}
	}

	return nil
}

// Modes returns the list of supported display modes.
// These are read from `/etc/fb.modes`.
//
// This can be called before Canvas.Open has been run.
func (c *Canvas) Modes() ([]*DisplayMode, error) {
	var (
		list         []*DisplayMode
		reg_endmode  = regexp.MustCompile(`endmode`)
		reg_label    = regexp.MustCompile(`^mode "([^"]+)"`)
		reg_geometry = regexp.MustCompile(`geometry (\d+) (\d+) (\d+) (\d+) (\d+)`)
		reg_timings  = regexp.MustCompile(`timings (\d+) (\d+) (\d+) (\d+) (\d+) (\d+) (\d+)`)
		reg_hsync    = regexp.MustCompile(`hsync high`)
		reg_vsync    = regexp.MustCompile(`vsync high`)
		reg_csync    = regexp.MustCompile(`csync high`)
		reg_extsync  = regexp.MustCompile(`extsync true`)
		reg_laced    = regexp.MustCompile(`laced true`)
		reg_double   = regexp.MustCompile(`double true`)
	)

	fd, err := os.Open("/etc/fb.modes")
	if err != nil {
		return nil, err
	}

	defer fd.Close()

	r := bufio.NewReader(fd)
	dm := new(DisplayMode)

	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break
		}

		// End of mode?
		if reg_endmode.Match(line) {
			list = append(list, dm)
			dm = new(DisplayMode)
			continue
		}

		// Parse label.
		matches := reg_label.FindSubmatch(line)
		if len(matches) == 2 {
			dm.Name = string(matches[1])
			continue
		}

		// Parse hsync
		matches = reg_hsync.FindSubmatch(line)
		if len(matches) == 2 {
			dm.Sync |= SyncHorHighAct
			continue
		}

		// Parse vsync
		matches = reg_vsync.FindSubmatch(line)
		if len(matches) == 2 {
			dm.Sync |= SyncVertHighAct
			continue
		}

		// Parse csync
		matches = reg_csync.FindSubmatch(line)
		if len(matches) == 2 {
			dm.Sync |= SyncCompHighAct
			continue
		}

		// Parse extsync
		matches = reg_extsync.FindSubmatch(line)
		if len(matches) == 2 {
			dm.Sync |= SyncExt
			continue
		}

		// Parse laced
		matches = reg_laced.FindSubmatch(line)
		if len(matches) == 2 {
			dm.VMode |= VModeInterlaced
			continue
		}

		// Parse double
		matches = reg_double.FindSubmatch(line)
		if len(matches) == 2 {
			dm.VMode |= VModeDouble
			continue
		}

		// Parse geometry
		matches = reg_geometry.FindSubmatch(line)
		if len(matches) == 6 {
			a, err := strconv.ParseInt(string(matches[1]), 10, 32)
			if err != nil {
				return nil, err
			}

			b, err := strconv.ParseInt(string(matches[2]), 10, 32)
			if err != nil {
				return nil, err
			}

			c, err := strconv.ParseInt(string(matches[3]), 10, 32)
			if err != nil {
				return nil, err
			}

			d, err := strconv.ParseInt(string(matches[4]), 10, 32)
			if err != nil {
				return nil, err
			}

			e, err := strconv.ParseInt(string(matches[5]), 10, 32)
			if err != nil {
				return nil, err
			}

			dm.XRes = int(a)
			dm.YRes = int(b)
			dm.XVRes = int(c)
			dm.YVRes = int(d)
			dm.Bpp = int(e)
			continue
		}

		// Parse timings
		matches = reg_timings.FindSubmatch(line)
		if len(matches) == 8 {
			a, err := strconv.ParseInt(string(matches[1]), 10, 32)
			if err != nil {
				return nil, err
			}

			b, err := strconv.ParseInt(string(matches[2]), 10, 32)
			if err != nil {
				return nil, err
			}

			c, err := strconv.ParseInt(string(matches[3]), 10, 32)
			if err != nil {
				return nil, err
			}

			d, err := strconv.ParseInt(string(matches[4]), 10, 32)
			if err != nil {
				return nil, err
			}

			e, err := strconv.ParseInt(string(matches[5]), 10, 32)
			if err != nil {
				return nil, err
			}

			f, err := strconv.ParseInt(string(matches[6]), 10, 32)
			if err != nil {
				return nil, err
			}

			g, err := strconv.ParseInt(string(matches[7]), 10, 32)
			if err != nil {
				return nil, err
			}

			dm.Pixclock = int(a)
			dm.Left = int(b)
			dm.Right = int(c)
			dm.Upper = int(d)
			dm.Lower = int(e)
			dm.HSync = int(f)
			dm.VSync = int(g)
			continue
		}
	}

	return list, nil
}

// Palette returns the current framebuffer color palette.
func (c *Canvas) Palette() (color.Palette, error) {
	var cm fb_cmap

	cm.start = 0
	cm.len = 256
	cm.red = unsafe.Pointer(&c.tmp_r[0])
	cm.green = unsafe.Pointer(&c.tmp_g[0])
	cm.blue = unsafe.Pointer(&c.tmp_b[0])
	cm.transp = unsafe.Pointer(&c.tmp_a[0])

	if ioctl(c.fd.Fd(), _IOGET_CMAP, unsafe.Pointer(&cm)) != nil {
		return nil, errors.New("Canvas.Palette failed")
	}

	s := int(cm.start)
	pal := make(color.Palette, cm.len)

	for i := range pal {
		pal[i] = color.NRGBA{
			uint8(c.tmp_r[i+s] >> 8),
			uint8(c.tmp_g[i+s] >> 8),
			uint8(c.tmp_b[i+s] >> 8),
			uint8(c.tmp_a[i+s] >> 8),
		}
	}

	return pal, nil
}

// SetPalette sets the current framebuffer color palette.
func (c *Canvas) SetPalette(pal color.Palette) error {
	if len(pal) >= 256 {
		pal = pal[:256]
	}

	for i, clr := range pal {
		r, g, b, a := clr.RGBA()
		c.tmp_r[i] = uint16(r >> 16)
		c.tmp_g[i] = uint16(g >> 16)
		c.tmp_b[i] = uint16(b >> 16)
		c.tmp_a[i] = uint16(a >> 16)
	}

	var cm fb_cmap
	cm.start = 0
	cm.len = 256
	cm.red = unsafe.Pointer(&c.tmp_r[0])
	cm.green = unsafe.Pointer(&c.tmp_g[0])
	cm.blue = unsafe.Pointer(&c.tmp_b[0])
	cm.transp = unsafe.Pointer(&c.tmp_a[0])

	if ioctl(c.fd.Fd(), _IOPUT_CMAP, unsafe.Pointer(&cm)) != nil {
		return errors.New("Canvas.SetPalette failed")
	}

	return nil
}

func (c *Canvas) switchAcquire() {
	ioctl(c.tty.Fd(), _VT_RELDISP, _VT_ACKACQ)
	c.switch_state = _FB_ACTIVE
}

func (c *Canvas) switchRelease() {
	ioctl(c.tty.Fd(), _VT_RELDISP, 1)
	c.switch_state = _FB_INACTIVE
}

func (c *Canvas) switchInit() error {
	var vm vt_mode

	vm.mode = _VT_PROCESS
	vm.waitv = 0
	vm.relsig = int16(syscall.SIGUSR1)
	vm.acqsig = int16(syscall.SIGUSR2)

	return ioctl(c.tty.Fd(), _VT_SETMODE, unsafe.Pointer(&vm))
}

// pollSignals polls for user signals.
func (c *Canvas) pollSignals() {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGUSR1, syscall.SIGUSR2)

	for sig := range signals {
		switch sig {
		case syscall.SIGUSR1: // Release
			c.switch_state = _FB_REL_REQ

		case syscall.SIGUSR2: // Acquire
			c.switch_state = _FB_ACQ_REQ
		}
	}
}

func (c *Canvas) activateCurrent(tty *os.File) error {
	var vts vt_stat

	err := ioctl(tty.Fd(), _VT_GETSTATE, unsafe.Pointer(&vts))
	if err != nil {
		return err
	}

	err = ioctl(tty.Fd(), _VT_ACTIVATE, int(vts.v_active))
	if err != nil {
		return err
	}

	return ioctl(tty.Fd(), _VT_WAITACTIVE, int(vts.v_active))
}
