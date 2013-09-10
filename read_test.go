// This file is subject to a 1-clause BSD license.
// Its contents can be found in the enclosed LICENSE file.

package framebuffer

import (
	"os"
	"testing"
)

func Test(t *testing.T) {
	fd, err := os.Open("/etc/fb.modes")
	if err != nil {
		t.Fatal(err)
	}

	defer fd.Close()

	_, err = readFBModes(fd)
	if err != nil {
		t.Error(err)
		return
	}
}
