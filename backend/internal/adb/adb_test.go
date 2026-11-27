package adb

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fakeFileInfo struct {
	name string
	dir  bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0o755 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

func TestFindOrderAndEnvOverride(t *testing.T) {
	norm := func(p string) string {
		return strings.ToLower(filepath.Clean(p))
	}

	t.Run("MIRRPANE_ADB authoritative valid", func(t *testing.T) {
		want := filepath.Join("D:", "tools", "adb.exe")
		got, err := findWith(findDeps{
			goos:   "windows",
			getenv: func(k string) string { return map[string]string{"MIRRPANE_ADB": want}[k] },
			lookPath: func(string) (string, error) {
				return `C:\other\adb.exe`, nil
			},
			executable: func() (string, error) { return `C:\app\mirrpane.exe`, nil },
			stat: func(p string) (os.FileInfo, error) {
				if norm(p) == norm(want) {
					return fakeFileInfo{name: "adb.exe"}, nil
				}
				return nil, os.ErrNotExist
			},
			glob: func(string) ([]string, error) { return nil, nil },
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if norm(got) != norm(want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("MIRRPANE_ADB bad path does not fall back", func(t *testing.T) {
		_, err := findWith(findDeps{
			goos:   "windows",
			getenv: func(k string) string { return map[string]string{"MIRRPANE_ADB": `Z:\missing\adb.exe`}[k] },
			lookPath: func(string) (string, error) {
				return `C:\path\adb.exe`, nil
			},
			executable: func() (string, error) { return `C:\app\mirrpane.exe`, nil },
			stat: func(p string) (os.FileInfo, error) {
				if norm(p) == norm(`C:\path\adb.exe`) {
					return fakeFileInfo{name: "adb.exe"}, nil
				}
				return nil, os.ErrNotExist
			},
			glob: func(string) ([]string, error) { return nil, nil },
		})
		if err == nil {
			t.Fatal("expected error for invalid MIRRPANE_ADB, got nil")
		}
		if !strings.Contains(err.Error(), "MIRRPANE_ADB") {
			t.Fatalf("error %q should mention MIRRPANE_ADB", err.Error())
		}
	})

	t.Run("PATH before WinGet and SDK", func(t *testing.T) {
		pathADB := filepath.Join("C:", "bin", "adb.exe")
		wingetADB := filepath.Join("C:", "Users", "u", "AppData", "Local", "Microsoft", "WinGet", "Packages", "Google.PlatformTools_1", "platform-tools", "adb.exe")
		got, err := findWith(findDeps{
			goos:       "windows",
			getenv:     func(k string) string { return map[string]string{"LOCALAPPDATA": `C:\Users\u\AppData\Local`}[k] },
			lookPath:   func(string) (string, error) { return pathADB, nil },
			executable: func() (string, error) { return "", os.ErrNotExist },
			stat: func(p string) (os.FileInfo, error) {
				if norm(p) == norm(pathADB) || norm(p) == norm(wingetADB) {
					return fakeFileInfo{name: "adb.exe"}, nil
				}
				return nil, os.ErrNotExist
			},
			glob: func(string) ([]string, error) { return []string{wingetADB}, nil },
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if norm(got) != norm(pathADB) {
			t.Fatalf("got %q, want %q", got, pathADB)
		}
	})

	t.Run("WinGet glob then SDK then UserProfile then ExeDir then C:\\platform-tools", func(t *testing.T) {
		localApp := filepath.Join("C:", "Users", "u", "AppData", "Local")
		userProf := filepath.Join("C:", "Users", "u2")
		exePath := filepath.Join("D:", "MirrPane", "mirrpane.exe")

		wingetADB := filepath.Join(localApp, "Microsoft", "WinGet", "Packages", "Google.PlatformTools_abc", "platform-tools", "adb.exe")
		sdkADB := filepath.Join(localApp, "Android", "Sdk", "platform-tools", "adb.exe")
		profSDKADB := filepath.Join(userProf, "AppData", "Local", "Android", "Sdk", "platform-tools", "adb.exe")
		exeADB := filepath.Join(filepath.Dir(exePath), "platform-tools", "adb.exe")
		rootADB := `C:\platform-tools\adb.exe`

		ordered := []string{wingetADB, sdkADB, profSDKADB, exeADB, rootADB}
		for i, target := range ordered {
			present := map[string]bool{}
			for _, candidate := range ordered[i:] {
				present[norm(candidate)] = true
			}
			got, err := findWith(findDeps{
				goos: "windows",
				getenv: func(k string) string {
					return map[string]string{
						"LOCALAPPDATA": localApp,
						"USERPROFILE":  userProf,
					}[k]
				},
				lookPath:   func(string) (string, error) { return "", errors.New("not in PATH") },
				executable: func() (string, error) { return exePath, nil },
				stat: func(p string) (os.FileInfo, error) {
					if present[norm(p)] {
						return fakeFileInfo{name: "adb.exe"}, nil
					}
					return nil, os.ErrNotExist
				},
				glob: func(string) ([]string, error) {
					if present[norm(wingetADB)] {
						return []string{wingetADB}, nil
					}
					return nil, nil
				},
			})
			if err != nil {
				t.Fatalf("step %d: unexpected error: %v", i, err)
			}
			if norm(got) != norm(target) {
				t.Fatalf("step %d: got %q, want %q", i, got, target)
			}
		}
	})
}

func TestParseDevicesOutput(t *testing.T) {
	fixture := strings.Join([]string{
		"* daemon not running; starting now at tcp:5037",
		"* daemon started successfully",
		"adb server version (41) doesn't match this client (39); killing...",
		"List of devices attached",
		"R5CT123456A            device product:dm1q model:SM_S911B device:dm1q transport_id:1",
		"192.168.1.42:5555      device product:husky model:Pixel 8 Pro device:husky transport_id:2",
		"adb-R5CT123456A-aBcDeF._adb-tls-connect._tcp\tdevice product:panther model:Pixel 7 device:panther transport_id:3",
		"emulator-5554          offline transport_id:4",
		"FA79X1A00123           unauthorized usb:1-2 transport_id:5",
		"0123 4567              device model:Space Serial Phone transport_id:6",
		"",
	}, "\r\n")

	devices, err := ParseDevicesOutput(fixture)
	if err != nil {
		t.Fatalf("ParseDevicesOutput failed: %v", err)
	}

	want := []Device{
		{Serial: "R5CT123456A", State: "device", Model: "SM_S911B"},
		{Serial: "192.168.1.42:5555", State: "device", Model: "Pixel 8 Pro"},
		{Serial: "adb-R5CT123456A-aBcDeF._adb-tls-connect._tcp", State: "device", Model: "Pixel 7"},
		{Serial: "emulator-5554", State: "offline", Model: ""},
		{Serial: "FA79X1A00123", State: "unauthorized", Model: ""},
		{Serial: "0123 4567", State: "device", Model: "Space Serial Phone"},
	}

	if len(devices) != len(want) {
		t.Fatalf("got %d devices (%+v), want %d (%+v)", len(devices), devices, len(want), want)
	}
	for i := range want {
		if devices[i] != want[i] {
			t.Errorf("device[%d] = %+v, want %+v", i, devices[i], want[i])
		}
	}
}

func TestParseDevicesOutputEmptyAndErrors(t *testing.T) {
	emptyFixture := "List of devices attached\n\n"
	devs, err := ParseDevicesOutput(emptyFixture)
	if err != nil {
		t.Fatalf("unexpected error for empty list: %v", err)
	}
	if len(devs) != 0 {
		t.Fatalf("got %d devices, want 0", len(devs))
	}

	if _, err := ParseDevicesOutput("garbage output without header\n"); err == nil {
		t.Fatal("expected error when header is missing")
	}
}

func buildFakeADBBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	bin := filepath.Join(dir, "adb")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	code := `package main

import (
	"os"
	"strings"
	"time"
)

func main() {
	mode := os.Getenv("FAKE_ADB_MODE")
	args := os.Args[1:]
	joined := strings.Join(args, " ")
	switch {
	case joined == "devices -l":
		if mode == "devices_fail" {
			os.Stderr.WriteString("adb: failed to check server version\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("* daemon not running; starting now at tcp:5037\n")
		os.Stdout.WriteString("* daemon started successfully\n")
		os.Stdout.WriteString("List of devices attached\n")
		os.Stdout.WriteString("SER123\tdevice usb:1-1 model:Pixel 9 device:tokay transport_id:7\n")
	case len(args) >= 2 && args[0] == "connect":
		switch mode {
		case "connect_already":
			os.Stdout.WriteString("already connected to " + args[1] + "\n")
		case "connect_exit0_fail":
			os.Stdout.WriteString("cannot connect to " + args[1] + ": connection refused\n")
		case "connect_exit1_stderr":
			os.Stderr.WriteString("adb: connection refused by peer\n")
			os.Exit(1)
		default:
			os.Stdout.WriteString("connected to " + args[1] + "\n")
		}
	case len(args) >= 3 && args[0] == "pair":
		if mode == "pair_fail" {
			os.Stderr.WriteString("Failed: Wrong pairing code\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("Successfully paired to " + args[1] + " [guid=adb-123]\n")
	case len(args) >= 2 && args[0] == "disconnect":
		if mode == "disconnect_fail" {
			os.Stderr.WriteString("error: no such device '" + args[1] + "'\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("disconnected " + args[1] + "\n")
	case len(args) >= 5 && args[0] == "-s" && args[2] == "forward" && args[3] == "tcp:0":
		os.Stdout.WriteString("27183\n")
	case len(args) >= 4 && args[0] == "-s" && args[2] == "forward" && args[3] == "--list":
		os.Stdout.WriteString(args[1] + " tcp:27183 localabstract:scrcpy_12345678\n")
	case len(args) >= 5 && args[0] == "-s" && args[2] == "forward" && args[3] == "--remove":
		os.Exit(0)
	case len(args) >= 1 && args[0] == "sleep":
		time.Sleep(5 * time.Second)
	default:
		os.Stderr.WriteString("unexpected args: " + joined + "\n")
		os.Exit(2)
	}
}
`
	if err := os.WriteFile(src, []byte(code), 0o600); err != nil {
		t.Fatalf("write fake adb: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake adb: %v\n%s", err, out)
	}
	return bin
}

func TestClientWithFakeADB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := New(buildFakeADBBinary(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	devs, err := client.Devices(ctx)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devs) != 1 || devs[0].Serial != "SER123" || devs[0].Model != "Pixel 9" || devs[0].State != "device" {
		t.Fatalf("unexpected devices: %+v", devs)
	}

	t.Run("Devices error surfaces stderr", func(t *testing.T) {
		t.Setenv("FAKE_ADB_MODE", "devices_fail")
		_, err := client.Devices(ctx)
		if err == nil || !strings.Contains(err.Error(), "failed to check server version") {
			t.Fatalf("expected stderr in error, got: %v", err)
		}
	})

	t.Run("Connect success and already connected", func(t *testing.T) {
		t.Setenv("FAKE_ADB_MODE", "")
		if err := client.Connect(ctx, "192.168.1.10:5555"); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		t.Setenv("FAKE_ADB_MODE", "connect_already")
		if err := client.Connect(ctx, "192.168.1.10:5555"); err != nil {
			t.Fatalf("Connect already connected: %v", err)
		}
	})

	t.Run("Connect failure exit 0 and exit 1", func(t *testing.T) {
		t.Setenv("FAKE_ADB_MODE", "connect_exit0_fail")
		if err := client.Connect(ctx, "192.168.1.10:5555"); err == nil || !strings.Contains(err.Error(), "cannot connect") {
			t.Fatalf("expected error on exit 0 cannot connect, got: %v", err)
		}

		t.Setenv("FAKE_ADB_MODE", "connect_exit1_stderr")
		if err := client.Connect(ctx, "192.168.1.10:5555"); err == nil || !strings.Contains(err.Error(), "connection refused by peer") {
			t.Fatalf("expected stderr in error, got: %v", err)
		}
	})

	t.Run("Pair and Disconnect", func(t *testing.T) {
		t.Setenv("FAKE_ADB_MODE", "")
		if err := client.Pair(ctx, "192.168.1.10:37123", "123456"); err != nil {
			t.Fatalf("Pair: %v", err)
		}
		if err := client.Disconnect(ctx, "192.168.1.10:5555"); err != nil {
			t.Fatalf("Disconnect: %v", err)
		}

		t.Setenv("FAKE_ADB_MODE", "pair_fail")
		if err := client.Pair(ctx, "192.168.1.10:37123", "000000"); err == nil || !strings.Contains(err.Error(), "Wrong pairing code") {
			t.Fatalf("expected pair stderr error, got: %v", err)
		}

		t.Setenv("FAKE_ADB_MODE", "disconnect_fail")
		if err := client.Disconnect(ctx, "192.168.1.10:5555"); err == nil || !strings.Contains(err.Error(), "no such device") {
			t.Fatalf("expected disconnect stderr error, got: %v", err)
		}
	})

	t.Run("Forward helpers", func(t *testing.T) {
		t.Setenv("FAKE_ADB_MODE", "")
		port, err := client.Forward(ctx, "SER123", "tcp:0", "localabstract:scrcpy_12345678")
		if err != nil || port != 27183 {
			t.Fatalf("Forward = (%d, %v), want (27183, nil)", port, err)
		}
		entries, err := client.ForwardList(ctx, "SER123")
		if err != nil || len(entries) != 1 || entries[0].Local != "tcp:27183" {
			t.Fatalf("ForwardList = (%+v, %v)", entries, err)
		}
		if err := client.ForwardRemove(ctx, "SER123", "tcp:27183"); err != nil {
			t.Fatalf("ForwardRemove: %v", err)
		}
	})

	t.Run("Context timeout kills command", func(t *testing.T) {
		shortCtx, shortCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer shortCancel()
		_, _, err := client.run(shortCtx, "sleep")
		if err == nil {
			t.Fatal("expected timeout error")
		}
	})
}
