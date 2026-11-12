package audio

import "testing"

func TestKeepCameraSkipsMetadata(t *testing.T) {
	if !keepCamera("Integrated Camera") || !keepCamera("USB2.0 HD UVC WebCam") {
		t.Fatal("real cameras were dropped")
	}
	if keepCamera("") || keepCamera("Integrated Camera: Integrated Camera Metadata") || keepCamera("  metadata capture ") {
		t.Fatal("metadata was kept")
	}
}
