package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/miku0139/airplay-go/backend/internal/airplayd"
)

// appVersion is stamped by the packaging script with
// -ldflags "-X main.appVersion=<version>".
var appVersion = "dev"

func main() {
	// Self re-exec: the receiver is this same executable with SelfExecArg as
	// its first argument. Dispatch before the UI registers flags or takes the
	// single-instance mutex, so the child never opens a window and never
	// competes with its own UI.
	if len(os.Args) > 1 && os.Args[1] == airplayd.SelfExecArg {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		airplayd.Main()
		return
	}
	installDirFlag := flag.String("install-dir", "", "override the installation directory holding config\\ and logs\\ (default: the executable's folder)")
	shmFlag := flag.String("shm", fmt.Sprintf(`Local\MirrPaneFrames-%d`, os.Getpid()), "shared-memory section for this receiver and its viewer")
	demoFlag := flag.Bool("demo", false, "render a synthetic test picture instead of the receiver's frames (no sidecar)")
	demoAndroid := flag.Bool("demo-android", false, "with -demo, present an Android (scrcpy) session for UI testing")
	demoSettings := flag.Bool("demo-settings", false, "open the settings dialog at startup (UI testing)")
	flag.Parse()

	// One receiver per logon session: a second launch focuses the running
	// window instead of starting another sidecar that would fight over the
	// AirPlay port and advertise a duplicate name.
	releaseInstance, ok := acquireSingleInstance()
	if !ok {
		return
	}
	defer releaseInstance()

	installDir := resolveInstallDir(*installDirFlag)
	var frameSrc FrameSource
	if *demoFlag {
		frameSrc = NewDemoFrameSource(2560, 1178)
	} else {
		frameSrc = newPlatformFrameSource(*shmFlag)
	}
	app := newMirrPaneApp(installDir, frameSrc, *demoFlag)
	app.demoAndroid = *demoAndroid && *demoFlag
	app.sidecar.mapping = *shmFlag
	if *demoSettings {
		app.openSettings()
	}

	mygo.App.SetName("鏡屏 MirrPane")
	mygo.App.OnWindowAllClosed(func() {
		mygo.App.Quit()
	})
	mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) {
		app.shutdown()
	})

	mygo.App.WhenReady(func() {
		win := mygo.NewWindow(mygo.WindowOptions{
			Title:           "鏡屏 MirrPane",
			Width:           1280,
			Height:          800,
			MinWidth:        640,
			MinHeight:       420,
			BackgroundColor: "#110F0E",
			Content:         ui.View(app.view),
		})
		app.attachWindow(win)
	})

	if err := mygo.App.Run(); err != nil {
		app.shutdown()
		log.Fatalf("mirrpane: %v", err)
	}
	app.shutdown()
}

// resolveInstallDir finds the folder that holds config\ and logs\. Release
// builds run MirrPane.exe from that folder; development launches can point
// -install-dir at a staged build. A damaged portable installation must report
// its own location.
func resolveInstallDir(explicit string) string {
	if explicit != "" {
		if abs, err := filepath.Abs(explicit); err == nil {
			return abs
		}
		return explicit
	}

	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}
