package files

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseMounts(t *testing.T) {
	mockMounts := []byte(`
sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
udev /dev devtmpfs rw,nosuid,noexec,relatime,size=8105740k,nr_inodes=2026435,mode=755 0 0
/dev/nvme0n1p2 / ext4 rw,relatime,errors=remount-ro 0 0
/dev/nvme0n1p3 /home ext4 rw,relatime 0 0
/dev/loop0 /snap/core/12345 squashfs ro,nodev,relatime 0 0
/dev/sdb1 /media/user/USB\040Drive vfat rw,nosuid,nodev,relatime,fmask=0022,dmask=0022,codepage=437,iocharset=iso8859-1,shortname=mixed,showexec,utf8,errors=remount-ro 0 0
/dev/sdc1 /mnt/backup ext4 rw,relatime 0 0
tmpfs /run/user/1000 tmpfs rw,nosuid,nodev,relatime,size=1626244k,nr_inodes=406561,mode=700,uid=1000,gid=1000 0 0
`)

	drives := ParseMounts(mockMounts)

	if len(drives) < 4 {
		t.Fatalf("expected at least 4 drives, got %d: %+v", len(drives), drives)
	}

	foundRoot := false
	foundHome := false
	foundUSB := false
	foundBackup := false

	for _, d := range drives {
		if d.MountPoint == "/" {
			foundRoot = true
			if d.FSType != "ext4" {
				t.Errorf("expected root fstype ext4, got %s", d.FSType)
			}
		}
		if d.MountPoint == "/home" {
			foundHome = true
		}
		if d.MountPoint == "/media/user/USB Drive" {
			foundUSB = true
			if !d.Removable {
				t.Errorf("expected USB drive to be marked removable")
			}
			if d.FSType != "vfat" {
				t.Errorf("expected USB fstype vfat, got %s", d.FSType)
			}
		}
		if d.MountPoint == "/mnt/backup" {
			foundBackup = true
		}
		if d.MountPoint == "/snap/core/12345" {
			t.Errorf("expected snap mount to be filtered out")
		}
	}

	if !foundRoot {
		t.Errorf("expected root / mount")
	}
	if !foundHome {
		t.Errorf("expected /home mount")
	}
	if !foundUSB {
		t.Errorf("expected USB drive mount with unescaped space")
	}
	if !foundBackup {
		t.Errorf("expected /mnt/backup mount")
	}
}

func TestFindDriveForPath(t *testing.T) {
	tempDir := t.TempDir()
	f, err := New(tempDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Should not panic and return a drive
	d := f.FindDriveForPath("/")
	if d == nil {
		t.Fatalf("expected drive for /, got nil")
	}

	dSub := f.FindDriveForPath("/home/user/Documents")
	if dSub == nil {
		t.Fatalf("expected drive for subfolder, got nil")
	}
}

func TestPlacesAndDrivesEndpoints(t *testing.T) {
	tempDir := t.TempDir()
	f, err := New(tempDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	mux := http.NewServeMux()
	noopAuth := func(h http.Handler) http.Handler { return h }
	f.Register(mux, noopAuth)

	// 1. GET /api/files/places
	{
		req := httptest.NewRequest("GET", "/api/files/places", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/files/places returned code %d", rec.Code)
		}
		var out struct {
			Places []Place `json:"places"`
			Drives []Drive `json:"drives"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal places: %v", err)
		}
		if len(out.Places) == 0 {
			t.Errorf("expected places, got none")
		}
		if len(out.Drives) == 0 {
			t.Errorf("expected drives, got none")
		}
	}

	// 2. GET /api/files/drives
	{
		req := httptest.NewRequest("GET", "/api/files/drives", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/files/drives returned code %d", rec.Code)
		}
		var out struct {
			Drives []Drive `json:"drives"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal drives: %v", err)
		}
		if len(out.Drives) == 0 {
			t.Errorf("expected drives, got none")
		}
	}

	// 3. GET /api/files/stat?path=/
	{
		req := httptest.NewRequest("GET", "/api/files/stat?path=/", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/files/stat returned code %d", rec.Code)
		}
		var out Drive
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal stat: %v", err)
		}
		if out.Total == 0 {
			t.Errorf("expected total > 0")
		}
	}
}
